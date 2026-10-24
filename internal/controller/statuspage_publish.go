/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The public page: index.html (static, rewritten only when the spec changes) and status.json
// (rewritten every run). The page reads status.json in the browser, so it never needs a server.
// status.json carries states and dates only: a check's URL and failure message stay in the
// resource's status and the alerts, never on the public page.

type statusJSONComponent struct {
	Name    string                        `json:"name"`
	Title   string                        `json:"title"`
	Group   string                        `json:"group,omitempty"`
	State   string                        `json:"state"`
	Since   string                        `json:"since,omitempty"`
	History []oramav1alpha1.StatusPageDay `json:"history"`
}

type statusJSONDocument struct {
	Title           string                `json:"title"`
	Generated       string                `json:"generated"`
	IntervalSeconds int32                 `json:"intervalSeconds"`
	Phase           string                `json:"phase"`
	Components      []statusJSONComponent `json:"components"`
}

func renderStatusJSON(page *oramav1alpha1.StatusPage, status *oramav1alpha1.StatusPageStatus, now time.Time) ([]byte, error) {
	doc := statusJSONDocument{
		Title: page.Spec.Title, Generated: now.Format(time.RFC3339),
		IntervalSeconds: defaultInt32(page.Spec.IntervalSeconds, 60), Phase: status.Phase,
	}
	observed := map[string]oramav1alpha1.StatusPageComponentStatus{}
	for _, c := range status.Components {
		observed[c.Name] = c
	}
	for _, spec := range page.Spec.Components {
		c := observed[spec.Name]
		item := statusJSONComponent{Name: spec.Name, Title: spec.Title, Group: spec.Group, State: c.State, History: c.History}
		if item.State == "" {
			item.State = statusUnknown
		}
		if item.History == nil {
			item.History = []oramav1alpha1.StatusPageDay{}
		}
		if c.Since != nil {
			item.Since = c.Since.UTC().Format(time.RFC3339)
		}
		doc.Components = append(doc.Components, item)
	}
	return json.MarshalIndent(doc, "", "  ")
}

var statusIndexTemplate = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{--bg:#fff;--fg:#14161a;--muted:#5b6170;--line:#e4e6eb;--ok:#1f9d55;--deg:#d69e2e;--out:#d64545;--unk:#a0a4ad}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--fg:#e8eaee;--muted:#9aa0ab;--line:#262a33;--unk:#5c616b}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
main{max-width:860px;margin:0 auto;padding:40px 20px}h1{font-size:24px;margin:0 0 8px}
.banner{border-radius:8px;padding:14px 16px;margin:20px 0;color:#fff;font-weight:600}
.Operational{background:var(--ok)}.Degraded{background:var(--deg)}.Outage{background:var(--out)}.Unknown{background:var(--unk)}
h2{font-size:15px;color:var(--muted);text-transform:uppercase;letter-spacing:.04em;margin:28px 0 8px}
.row{border:1px solid var(--line);border-radius:8px;padding:12px 14px;margin:8px 0}
.head{display:flex;justify-content:space-between;gap:12px}.state{font-weight:600}
.state.Operational{color:var(--ok);background:none}.state.Degraded{color:var(--deg);background:none}
.state.Outage{color:var(--out);background:none}.state.Unknown{color:var(--unk);background:none}
.bars{display:flex;gap:2px;margin-top:10px;height:28px}.bars span{flex:1;border-radius:2px;min-width:2px}
.since,footer{color:var(--muted);font-size:13px}footer{margin-top:32px}
</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
<div id="banner" class="banner Unknown">Loading the current status…</div>
<div id="components"></div>
<footer id="footer">Checked every {{.Interval}} seconds from outside the service.</footer>
</main>
<script>
(function(){
var interval={{.Interval}};
var words={Operational:"All systems operational",Degraded:"Some systems degraded",Outage:"Outage in progress",Unknown:"Status unknown"};
function el(tag,cls,text){var e=document.createElement(tag);if(cls)e.className=cls;if(text!=null)e.textContent=text;return e}
function days(history){var byDate={};(history||[]).forEach(function(d){byDate[d.date]=d.state});var out=[];var now=new Date();
for(var i=89;i>=0;i--){var d=new Date(Date.UTC(now.getUTCFullYear(),now.getUTCMonth(),now.getUTCDate()-i));var k=d.toISOString().slice(0,10);out.push({date:k,state:byDate[k]||"none"})}return out}
function render(doc){
  var stale=(Date.now()-Date.parse(doc.generated))/1000>3*interval+60;
  var phase=stale?"Unknown":doc.phase||"Unknown";
  var banner=document.getElementById("banner");banner.className="banner "+phase;
  banner.textContent=stale?"Status unknown: the checks have not reported since "+doc.generated.replace("T"," ").replace("Z"," UTC"):words[phase];
  var root=document.getElementById("components");root.textContent="";var group=null;
  doc.components.forEach(function(c){
    if((c.group||"")!==group){group=c.group||"";if(group)root.appendChild(el("h2",null,group))}
    var row=el("div","row");var head=el("div","head");head.appendChild(el("span",null,c.title));
    var state=stale?"Unknown":c.state;head.appendChild(el("span","state "+state,state));row.appendChild(head);
    if(c.since&&!stale)row.appendChild(el("div","since","since "+c.since.replace("T"," ").replace("Z"," UTC")));
    var bars=el("div","bars");days(c.history).forEach(function(d){var s=el("span");s.title=d.date+": "+(d.state==="none"?"no data":d.state);
      s.style.background=d.state==="none"?"var(--line)":"var(--"+({Operational:"ok",Degraded:"deg",Outage:"out"}[d.state]||"unk")+")";bars.appendChild(s)});
    row.appendChild(bars);root.appendChild(row)});
  document.getElementById("footer").textContent="Checked every "+interval+" seconds from outside the service. Last check: "+doc.generated.replace("T"," ").replace("Z"," UTC")+". 90 days shown, worst state per day (UTC).";
}
function load(){fetch("status.json",{cache:"no-store"}).then(function(r){if(!r.ok)throw new Error(r.status);return r.json()}).then(render).catch(function(){
  var b=document.getElementById("banner");b.className="banner Unknown";b.textContent="Status unknown: the status could not be read"})}
load();setInterval(load,interval*1000);
})();
</script>
</body>
</html>
`))

func renderStatusIndex(page *oramav1alpha1.StatusPage) []byte {
	var out bytes.Buffer
	_ = statusIndexTemplate.Execute(&out, struct {
		Title    string
		Interval int32
	}{page.Spec.Title, defaultInt32(page.Spec.IntervalSeconds, 60)})
	return out.Bytes()
}

// statusMailAccount is an SMTP account and the alert's recipients, read from a Secret.
type statusMailAccount struct {
	Host, Port, Username, Password, From string
	To                                   []string
}

func statusMailAccountFrom(data map[string][]byte) (statusMailAccount, error) {
	get := func(key string) string { return strings.TrimSpace(string(data[key])) }
	account := statusMailAccount{Host: get("host"), Port: get("port"), Username: get("username"), Password: get("password"), From: get("from")}
	for _, to := range strings.Split(get("to"), ",") {
		if to = strings.TrimSpace(to); to != "" {
			account.To = append(account.To, to)
		}
	}
	if account.Port == "" {
		account.Port = "465"
	}
	for key, value := range map[string]string{"host": account.Host, "username": account.Username, "password": account.Password, "from": account.From} {
		if value == "" {
			return statusMailAccount{}, fmt.Errorf("no key %s", key)
		}
	}
	if len(account.To) == 0 {
		return statusMailAccount{}, errors.New("no key to")
	}
	return account, nil
}

// statusMailMessage is the RFC 5322 message an email alert sends.
func statusMailMessage(account statusMailAccount, subject, body string, now time.Time) []byte {
	clean := func(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }
	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s\r\n", clean(account.From))
	fmt.Fprintf(&msg, "To: %s\r\n", clean(strings.Join(account.To, ", ")))
	fmt.Fprintf(&msg, "Subject: %s\r\n", clean(subject))
	fmt.Fprintf(&msg, "Date: %s\r\n", now.Format(time.RFC1123Z))
	msg.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	msg.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	msg.WriteString("\r\n")
	return []byte(msg.String())
}

// sendStatusMail sends over SMTP: implicit TLS on port 465, STARTTLS (required) on any other.
// The account never travels in clear text.
func sendStatusMail(ctx context.Context, account statusMailAccount, subject, body string) error {
	address := net.JoinHostPort(account.Host, account.Port)
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tlsConfig := &tls.Config{ServerName: account.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	var err error
	if account.Port == "465" {
		conn, err = tls.DialWithDialer(dialer, "tcp", address, tlsConfig)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, account.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if account.Port != "465" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the SMTP server does not offer STARTTLS")
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if err := c.Auth(smtp.PlainAuth("", account.Username, account.Password, account.Host)); err != nil {
		return err
	}
	from := account.From
	if parsed, err := mail.ParseAddress(account.From); err == nil {
		from = parsed.Address
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, to := range account.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(statusMailMessage(account, subject, body, time.Now().UTC())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
