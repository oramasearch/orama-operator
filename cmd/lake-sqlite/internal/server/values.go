/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// base64Key marks a blob in both directions: {"$base64": "<standard base64>"}.
const base64Key = "$base64"

// decodeParams turns the JSON params array into driver arguments. The decoder ran with UseNumber,
// so numbers arrive as json.Number: an integer literal binds as int64 and anything with a fraction
// or exponent as float64. Booleans bind as 1 or 0, SQLite having no boolean type. A blob is an
// object with the single key "$base64". Any other array or object is refused.
func decodeParams(raw []interface{}) ([]interface{}, error) {
	args := make([]interface{}, len(raw))
	for i, value := range raw {
		arg, err := decodeParam(value)
		if err != nil {
			return nil, fmt.Errorf("params[%d]: %v", i, err)
		}
		args[i] = arg
	}
	return args, nil
}

func decodeParam(value interface{}) (interface{}, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		return v, nil
	case bool:
		if v {
			return int64(1), nil
		}
		return int64(0), nil
	case json.Number:
		text := v.String()
		if !strings.ContainsAny(text, ".eE") {
			if n, err := strconv.ParseInt(text, 10, 64); err == nil {
				return n, nil
			}
			return nil, fmt.Errorf("integer %s does not fit in 64 bits", text)
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("number %s is not a 64-bit float", text)
		}
		return f, nil
	case map[string]interface{}:
		encoded, ok := v[base64Key].(string)
		if len(v) != 1 || !ok {
			return nil, fmt.Errorf(`an object parameter must be {"%s": "<base64>"}`, base64Key)
		}
		blob, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid base64: %v", err)
		}
		return blob, nil
	default:
		return nil, fmt.Errorf("unsupported parameter type %T", value)
	}
}

// sqlFloat keeps a REAL a float in JSON: 1.0 is written 1.0, not 1, so a client can tell it from
// an INTEGER. SQLite can hold an infinity, which JSON cannot; it is written as the string
// "Infinity" or "-Infinity".
type sqlFloat float64

func (f sqlFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	switch {
	case math.IsInf(v, 1):
		return []byte(`"Infinity"`), nil
	case math.IsInf(v, -1):
		return []byte(`"-Infinity"`), nil
	case math.IsNaN(v):
		// SQLite stores NaN as NULL, so this is not reached in practice.
		return []byte("null"), nil
	}
	text := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return []byte(text), nil
}

// blob is a BLOB value: {"$base64": "..."}.
type blob []byte

func (b blob) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"` + base64Key + `":"`)
	buf.WriteString(base64.StdEncoding.EncodeToString(b))
	buf.WriteString(`"}`)
	return buf.Bytes(), nil
}

// encodeValue maps a value the driver scanned to its JSON form: INTEGER to a JSON integer, REAL to
// a JSON float, TEXT to a string, BLOB to {"$base64": ...} and NULL to null.
//
// The driver reads TEXT stored in a column declared DATE, DATETIME or TIMESTAMP as a time when it
// parses as one; that value is written back as text in SQLite's own format. Declare the column
// TEXT to have every byte returned exactly as stored.
func encodeValue(value interface{}) interface{} {
	switch v := value.(type) {
	case nil:
		return nil
	case int64:
		return v
	case float64:
		return sqlFloat(v)
	case string:
		return v
	case []byte:
		return blob(v)
	case bool:
		if v {
			return int64(1)
		}
		return int64(0)
	case time.Time:
		return formatTime(v)
	default:
		return fmt.Sprint(v)
	}
}

// formatTime writes a time the driver parsed from TEXT in SQLite's date and time format: the date
// alone at midnight UTC, otherwise date and time with fractional seconds when there are any and a
// numeric offset when it is not UTC.
func formatTime(t time.Time) string {
	_, offset := t.Zone()
	if offset == 0 && t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		return t.Format("2006-01-02")
	}
	layout := "2006-01-02 15:04:05.999999999"
	if offset != 0 {
		layout += "-07:00"
	}
	return t.Format(layout)
}
