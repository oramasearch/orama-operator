/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package backup

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// A reader of the RDB format Valkey (and Redis) write, enough to count and copy every key
// (operator#736): it walks each entry, keeps its key, expiry and the value's own encoding as written,
// and never decodes a value. That encoding, followed by the RDB version and a CRC-64, is exactly the
// DUMP payload RESTORE takes, so a restore re-creates each value byte for byte. Module values are
// refused (their encoding is the module's), and so is any value type this reader does not know.

type rdbEntry struct {
	db       int
	key      []byte
	expireMS int64
	typ      byte
	value    []byte
	version  int
}

// dumpPayload is the entry as DUMP would give it: type, value, RDB version (2 bytes, little
// endian), CRC-64 (8 bytes, little endian) of everything before.
func (e rdbEntry) dumpPayload() []byte {
	payload := make([]byte, 0, len(e.value)+11)
	payload = append(payload, e.typ)
	payload = append(payload, e.value...)
	payload = binary.LittleEndian.AppendUint16(payload, uint16(e.version))
	return binary.LittleEndian.AppendUint64(payload, crc64Jones(0, payload))
}

const (
	rdbOpSlotInfo    = 0xF4
	rdbOpFunction2   = 0xF5
	rdbOpFunctionOld = 0xF6
	rdbOpModuleAux   = 0xF7
	rdbOpIdle        = 0xF8
	rdbOpFreq        = 0xF9
	rdbOpAux         = 0xFA
	rdbOpResizeDB    = 0xFB
	rdbOpExpireMS    = 0xFC
	rdbOpExpire      = 0xFD
	rdbOpSelectDB    = 0xFE
	rdbOpEOF         = 0xFF
)

// rdbReader reads from the stream and, while recording, keeps what it read.
type rdbReader struct {
	r         *bufio.Reader
	recording bool
	record    bytes.Buffer
}

func (r *rdbReader) byte() (byte, error) {
	b, err := r.r.ReadByte()
	if err == nil && r.recording {
		r.record.WriteByte(b)
	}
	return b, err
}

func (r *rdbReader) bytes(n int64) ([]byte, error) {
	if n < 0 || n > 1<<31 {
		return nil, fmt.Errorf("rdb: a length of %d", n)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r.r, data); err != nil {
		return nil, err
	}
	if r.recording {
		r.record.Write(data)
	}
	return data, nil
}

// length reads a length; encoded is true (and the value is the encoding) for a special string.
func (r *rdbReader) length() (uint64, bool, error) {
	b, err := r.byte()
	if err != nil {
		return 0, false, err
	}
	switch b >> 6 {
	case 0:
		return uint64(b & 0x3f), false, nil
	case 1:
		next, err := r.byte()
		return uint64(b&0x3f)<<8 | uint64(next), false, err
	case 2:
		switch b {
		case 0x80:
			data, err := r.bytes(4)
			if err != nil {
				return 0, false, err
			}
			return uint64(binary.BigEndian.Uint32(data)), false, nil
		case 0x81:
			data, err := r.bytes(8)
			if err != nil {
				return 0, false, err
			}
			return binary.BigEndian.Uint64(data), false, nil
		}
		return 0, false, fmt.Errorf("rdb: length byte %#x", b)
	}
	return uint64(b & 0x3f), true, nil
}

func (r *rdbReader) count() (int64, error) {
	n, encoded, err := r.length()
	if err == nil && encoded {
		err = errors.New("rdb: an encoded value where a count belongs")
	}
	return int64(n), err
}

// str reads a string, decoded (integers as decimal text, LZF decompressed).
func (r *rdbReader) str() ([]byte, error) {
	n, encoded, err := r.length()
	if err != nil {
		return nil, err
	}
	if !encoded {
		return r.bytes(int64(n))
	}
	switch n {
	case 0:
		data, err := r.bytes(1)
		if err != nil {
			return nil, err
		}
		return []byte(strconv.Itoa(int(int8(data[0])))), nil
	case 1:
		data, err := r.bytes(2)
		if err != nil {
			return nil, err
		}
		return []byte(strconv.Itoa(int(int16(binary.LittleEndian.Uint16(data))))), nil
	case 2:
		data, err := r.bytes(4)
		if err != nil {
			return nil, err
		}
		return []byte(strconv.Itoa(int(int32(binary.LittleEndian.Uint32(data))))), nil
	case 3:
		compressed, err := r.count()
		if err != nil {
			return nil, err
		}
		size, err := r.count()
		if err != nil {
			return nil, err
		}
		data, err := r.bytes(compressed)
		if err != nil {
			return nil, err
		}
		return lzfDecompress(data, int(size))
	}
	return nil, fmt.Errorf("rdb: string encoding %d", n)
}

func (r *rdbReader) skipStrings(n int64) error {
	for i := int64(0); i < n; i++ {
		if _, err := r.str(); err != nil {
			return err
		}
	}
	return nil
}

// value skips one value of type typ, recording it.
func (r *rdbReader) value(typ byte) error {
	switch typ {
	case 0, 9, 10, 11, 12, 13, 16, 17, 20: // string; zipmap, ziplists, intset and listpacks are one string
		_, err := r.str()
		return err
	case 1, 2, 14: // list, set, quicklist of ziplists: n strings
		n, err := r.count()
		if err != nil {
			return err
		}
		return r.skipStrings(n)
	case 4: // hash: n pairs
		n, err := r.count()
		if err != nil {
			return err
		}
		return r.skipStrings(2 * n)
	case 3: // zset: member, then a score as a length-prefixed decimal string (253-255: nan, inf, -inf)
		n, err := r.count()
		if err != nil {
			return err
		}
		for i := int64(0); i < n; i++ {
			if _, err := r.str(); err != nil {
				return err
			}
			size, err := r.byte()
			if err != nil {
				return err
			}
			if size < 253 {
				if _, err := r.bytes(int64(size)); err != nil {
					return err
				}
			}
		}
		return nil
	case 5: // zset 2: member, then an 8-byte binary double
		n, err := r.count()
		if err != nil {
			return err
		}
		for i := int64(0); i < n; i++ {
			if _, err := r.str(); err != nil {
				return err
			}
			if _, err := r.bytes(8); err != nil {
				return err
			}
		}
		return nil
	case 18: // quicklist 2: n containers of (kind, listpack)
		n, err := r.count()
		if err != nil {
			return err
		}
		for i := int64(0); i < n; i++ {
			if _, err := r.count(); err != nil {
				return err
			}
			if _, err := r.str(); err != nil {
				return err
			}
		}
		return nil
	case 15, 19, 21:
		return r.stream(typ)
	case 6, 7:
		return errors.New("rdb: a module value, which only its module can read; not backed up")
	}
	return fmt.Errorf("rdb: value type %d is not known to this reader", typ)
}

// stream skips a stream value (STREAM_LISTPACKS, _2, _3), as rdb.c writes it.
func (r *rdbReader) stream(typ byte) error {
	counts := func(n int) error {
		for i := 0; i < n; i++ {
			if _, err := r.count(); err != nil {
				return err
			}
		}
		return nil
	}
	nodes, err := r.count()
	if err != nil {
		return err
	}
	if err := r.skipStrings(2 * nodes); err != nil { // master id, listpack
		return err
	}
	if err := counts(3); err != nil { // length, last id ms, seq
		return err
	}
	if typ >= 19 {
		if err := counts(5); err != nil { // first id, max deleted id, entries added
			return err
		}
	}
	groups, err := r.count()
	if err != nil {
		return err
	}
	for g := int64(0); g < groups; g++ {
		if _, err := r.str(); err != nil {
			return err
		}
		if err := counts(2); err != nil {
			return err
		}
		if typ >= 19 {
			if err := counts(1); err != nil { // entries read
				return err
			}
		}
		pending, err := r.count()
		if err != nil {
			return err
		}
		for p := int64(0); p < pending; p++ {
			if _, err := r.bytes(16 + 8); err != nil { // raw id, delivery time
				return err
			}
			if err := counts(1); err != nil { // delivery count
				return err
			}
		}
		consumers, err := r.count()
		if err != nil {
			return err
		}
		for c := int64(0); c < consumers; c++ {
			if _, err := r.str(); err != nil {
				return err
			}
			times := int64(8)
			if typ >= 21 {
				times = 16 // seen time, active time
			}
			if _, err := r.bytes(times); err != nil {
				return err
			}
			owned, err := r.count()
			if err != nil {
				return err
			}
			if _, err := r.bytes(16 * owned); err != nil {
				return err
			}
		}
	}
	return nil
}

// readRDB reads an RDB stream to its EOF marker and checksum, calling each for every key, and
// returns the RDB version.
func readRDB(stream io.Reader, each func(rdbEntry) error) (int, error) {
	r := &rdbReader{r: bufio.NewReaderSize(stream, 1<<20)}
	magic, err := r.bytes(9)
	if err != nil {
		return 0, fmt.Errorf("rdb: no header: %w", err)
	}
	var version int
	switch {
	case bytes.HasPrefix(magic, []byte("REDIS")):
		version, err = strconv.Atoi(string(magic[5:]))
	case bytes.HasPrefix(magic, []byte("VALKEY")):
		version, err = strconv.Atoi(string(magic[6:]))
	default:
		err = errors.New("not REDIS or VALKEY")
	}
	if err != nil {
		return 0, fmt.Errorf("rdb: header %q: %v", magic, err)
	}
	db := 0
	var expire int64
	for {
		op, err := r.byte()
		if err != nil {
			return version, fmt.Errorf("rdb: cut short: %w", err)
		}
		switch op {
		case rdbOpEOF:
			if version >= 5 {
				_, err = r.bytes(8)
			}
			return version, err
		case rdbOpSelectDB:
			n, err := r.count()
			if err != nil {
				return version, err
			}
			db = int(n)
			continue
		case rdbOpResizeDB:
			if _, err := r.count(); err != nil {
				return version, err
			}
			if _, err := r.count(); err != nil {
				return version, err
			}
			continue
		case rdbOpSlotInfo:
			for i := 0; i < 3; i++ {
				if _, err := r.count(); err != nil {
					return version, err
				}
			}
			continue
		case rdbOpAux:
			if err := r.skipStrings(2); err != nil {
				return version, err
			}
			continue
		case rdbOpFunction2:
			if _, err := r.str(); err != nil {
				return version, err
			}
			continue
		case rdbOpFunctionOld, rdbOpModuleAux:
			return version, fmt.Errorf("rdb: opcode %#x (module or pre-release function data) is not backed up", op)
		case rdbOpExpireMS:
			data, err := r.bytes(8)
			if err != nil {
				return version, err
			}
			expire = int64(binary.LittleEndian.Uint64(data))
			continue
		case rdbOpExpire:
			data, err := r.bytes(4)
			if err != nil {
				return version, err
			}
			expire = int64(binary.LittleEndian.Uint32(data)) * 1000
			continue
		case rdbOpFreq:
			if _, err := r.bytes(1); err != nil {
				return version, err
			}
			continue
		case rdbOpIdle:
			if _, err := r.count(); err != nil {
				return version, err
			}
			continue
		}
		key, err := r.str()
		if err != nil {
			return version, err
		}
		r.recording = true
		r.record.Reset()
		err = r.value(op)
		r.recording = false
		if err != nil {
			return version, fmt.Errorf("key %q: %w", key, err)
		}
		entry := rdbEntry{db: db, key: key, expireMS: expire, typ: op, value: append([]byte(nil), r.record.Bytes()...), version: version}
		expire = 0
		if err := each(entry); err != nil {
			return version, err
		}
	}
}

// lzfDecompress is LibLZF's decompressor, which Valkey uses for long strings.
func lzfDecompress(in []byte, size int) ([]byte, error) {
	out := make([]byte, 0, size)
	for i := 0; i < len(in); {
		ctrl := int(in[i])
		i++
		if ctrl < 32 {
			n := ctrl + 1
			if i+n > len(in) {
				return nil, errors.New("lzf: literal past the end")
			}
			out = append(out, in[i:i+n]...)
			i += n
			continue
		}
		length := ctrl >> 5
		if length == 7 {
			if i >= len(in) {
				return nil, errors.New("lzf: truncated")
			}
			length += int(in[i])
			i++
		}
		if i >= len(in) {
			return nil, errors.New("lzf: truncated")
		}
		ref := len(out) - ((ctrl & 0x1f) << 8) - 1 - int(in[i])
		i++
		if ref < 0 {
			return nil, errors.New("lzf: back reference before the start")
		}
		for j := 0; j < length+2; j++ {
			out = append(out, out[ref+j])
		}
	}
	if len(out) != size {
		return nil, fmt.Errorf("lzf: %d bytes, expected %d", len(out), size)
	}
	return out, nil
}

// crc64Jones is the CRC-64 Valkey checks DUMP payloads and RDB files with (Jones polynomial,
// reflected, no final XOR).
func crc64Jones(crc uint64, data []byte) uint64 {
	for _, b := range data {
		crc = crc64JonesTable[byte(crc)^b] ^ (crc >> 8)
	}
	return crc
}

var crc64JonesTable = func() (table [256]uint64) {
	const reflected = 0x95AC9329AC4BC9B5
	for i := range table {
		crc := uint64(i)
		for j := 0; j < 8; j++ {
			if crc&1 == 1 {
				crc = crc>>1 ^ reflected
			} else {
				crc >>= 1
			}
		}
		table[i] = crc
	}
	return table
}()
