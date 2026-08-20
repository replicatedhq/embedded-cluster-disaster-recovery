package protocol

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

var backupTemplate = template.Must(template.New("backup").Parse(`<!doctype html>
<html lang="en"><head>
  <meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Disaster Recovery</title>
  <style nonce="{{.Nonce}}">
    body { margin: 0; background: #f5f7fb; color: #182230; font: 15px system-ui, sans-serif; }
    main { max-width: 900px; margin: 36px auto; padding: 0 20px 48px; }
    section { background: white; padding: 26px; margin: 18px 0; border-radius: 12px; box-shadow: 0 4px 18px #1d293914; }
    h1, h2 { margin-top: 0; } .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
    label { display: block; font-weight: 650; margin-bottom: 5px; }
    input, textarea { width: 100%; box-sizing: border-box; padding: 9px; border: 1px solid #cbd5e1; border-radius: 6px; }
    button { padding: 10px 15px; border: 0; border-radius: 6px; background: #175cd3; color: white; font-weight: 650; cursor: pointer; }
    button.secondary { background: #475467; } button:disabled { opacity: .5; cursor: wait; }
    .message { margin-top: 14px; padding: 11px; border-radius: 6px; background: #eff8ff; white-space: pre-wrap; }
    .key { display: none; background: #fff6ed; } table { width: 100%; border-collapse: collapse; }
    th, td { text-align: left; padding: 9px 6px; border-bottom: 1px solid #eaecf0; }
    @media (max-width: 700px) { .grid { grid-template-columns: 1fr; } }
  </style>
</head><body><main>
  <h1>Disaster Recovery</h1>
  <p>Configure S3-compatible storage, keep the recovery key outside this cluster, and create complete recovery points.</p>
  <section><h2>Backup storage</h2>
    <div class="grid">
      <div><label for="bucket">Bucket</label><input id="bucket" autocomplete="off"></div>
      <div><label for="prefix">Prefix</label><input id="prefix" value="embedded-cluster-dr" autocomplete="off"></div>
      <div><label for="region">Region</label><input id="region" value="auto" autocomplete="off"></div>
      <div><label for="endpoint">Custom S3 endpoint</label><input id="endpoint" placeholder="https://ACCOUNT.r2.cloudflarestorage.com" autocomplete="off"></div>
      <div><label for="accessKeyId">Access key ID</label><input id="accessKeyId" autocomplete="off"></div>
      <div><label for="secretAccessKey">Secret access key</label><input id="secretAccessKey" type="password" autocomplete="off"></div>
      <div><label for="schedule">Schedule (five-field cron, optional)</label><input id="schedule" placeholder="0 2 * * *" autocomplete="off"></div>
      <div><label for="retention">Recovery points to retain</label><input id="retention" type="number" min="1" max="1000" value="30"></div>
      <div><label for="proxyUrl">Proxy URL (optional)</label><input id="proxyUrl" autocomplete="off"></div>
      <div><label for="customCaPem">Custom CA certificate (optional)</label><textarea id="customCaPem" rows="4"></textarea></div>
    </div>
    <p><button id="save">Test and save configuration</button></p>
    <div id="configMessage" class="message">Loading configuration status…</div>
    <div id="keyMessage" class="message key"><strong>Download the recovery key now.</strong> It is shown only when first generated.<br><button id="downloadKey" class="secondary">Download recovery key</button></div>
  </section>
  <section><h2>Backups</h2>
    <button id="backup">Create backup</button>
    <div id="backupMessage" class="message">A recovery point is Ready only after EC state, Kubernetes resources, and volumes all succeed.</div>
    <table><thead><tr><th>Created</th><th>Recovery point</th></tr></thead><tbody id="points"></tbody></table>
  </section>
<script nonce="{{.Nonce}}">
const extensionBase = {{.ExtensionBase}};
const consoleBase = {{.ConsoleBase}};
let recoveryKey = '';
const value = id => document.getElementById(id).value;
function message(id, text) { document.getElementById(id).textContent = text; }
async function loadConfiguration() {
  const response = await fetch(extensionBase + '/ui/api/configuration'); const body = await response.json();
  if (!response.ok) { message('configMessage', body.message || 'Configuration is unavailable.'); return; }
  if (!body.configured) { message('configMessage', 'Not configured. Enter storage credentials to begin.'); return; }
  for (const field of ['bucket','prefix','region','endpoint','accessKeyId','schedule']) if (body[field] !== undefined) document.getElementById(field).value = body[field];
  if (body.retentionCount) document.getElementById('retention').value = body.retentionCount;
  message('configMessage', 'Configured' + (body.schedulePaused ? '; scheduled backups are paused after restore.' : '.'));
  await loadPoints();
}
async function saveConfiguration() {
  const button = document.getElementById('save'); button.disabled = true; message('configMessage', 'Testing storage and saving configuration…');
  const payload = {storage:{bucket:value('bucket'),prefix:value('prefix'),region:value('region'),endpoint:value('endpoint'),accessKeyId:value('accessKeyId'),secretAccessKey:value('secretAccessKey'),forcePathStyle:true,customCaPem:value('customCaPem'),proxyUrl:value('proxyUrl')},schedule:value('schedule'),retentionCount:Number(value('retention'))};
  const response = await fetch(extensionBase + '/ui/api/configuration',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload)}); const body = await response.json();
  button.disabled = false; document.getElementById('secretAccessKey').value = '';
  if (!response.ok) { message('configMessage', body.message || 'Configuration failed.'); return; }
  message('configMessage','Storage is writable and disaster recovery is configured.');
  if (body.recoveryKeyGenerated) { recoveryKey = body.recoveryKey; document.getElementById('keyMessage').style.display = 'block'; }
  await loadPoints();
}
function downloadKey() {
  if (!recoveryKey) return; const blob = new Blob([recoveryKey + '\n'],{type:'text/plain'}); const link = document.createElement('a');
  link.href = URL.createObjectURL(blob); link.download = 'embedded-cluster-recovery-key.txt'; link.click(); URL.revokeObjectURL(link.href);
}
async function createBackup() {
  const button = document.getElementById('backup'); button.disabled = true; message('backupMessage','Exporting Embedded Cluster state…');
  let response = await fetch(consoleBase + '/backup',{method:'POST',headers:{'Content-Type':'application/json'},body:'{}'}); let body = await response.json();
  if (!response.ok) { button.disabled=false; message('backupMessage',body.error || 'Backup could not be prepared.'); return; }
  const id = body.operationId; response = await fetch(consoleBase + '/operations/' + encodeURIComponent(id) + '/run',{method:'POST'}); body = await response.json();
  if (!response.ok) { button.disabled=false; message('backupMessage',body.error || 'Backup could not start.'); return; }
  while (true) { await new Promise(resolve => setTimeout(resolve,1500)); response = await fetch(consoleBase + '/operations/' + encodeURIComponent(id)); body = await response.json();
    if (body.progress) message('backupMessage',body.progress.message);
    if (body.state === 'succeeded') { message('backupMessage','Recovery point ' + body.result.recoveryPointId + ' is Ready.'); break; }
    if (body.state === 'failed' || body.state === 'canceled') { message('backupMessage',body.error ? body.error.message : 'Backup did not complete.'); break; }
  }
  button.disabled=false; await loadPoints();
}
async function loadPoints() {
  const response = await fetch(extensionBase + '/ui/api/recovery-points'); const body = await response.json(); if (!response.ok) return;
  const rows = document.getElementById('points'); rows.replaceChildren(); for (const point of body.recoveryPoints) { const row=document.createElement('tr'); const date=document.createElement('td'); date.textContent=new Date(point.createdAt).toLocaleString(); const id=document.createElement('td'); id.textContent=point.id; row.append(date,id); rows.appendChild(row); }
}
document.getElementById('save').addEventListener('click',saveConfiguration); document.getElementById('downloadKey').addEventListener('click',downloadKey); document.getElementById('backup').addEventListener('click',createBackup); loadConfiguration();
</script></main></body></html>`))

func (s *Server) backupUI(writer http.ResponseWriter, request *http.Request) {
	if s.settings == nil {
		http.Error(writer, "in-cluster disaster recovery UI is unavailable", http.StatusNotFound)
		return
	}
	nonce, err := uiNonce()
	if err != nil {
		http.Error(writer, "could not initialize disaster recovery UI", http.StatusInternalServerError)
		return
	}
	extensionBase := strings.TrimSuffix(request.Header.Get("X-Forwarded-Prefix"), "/")
	consoleBase := strings.TrimSuffix(extensionBase, "/extension")
	if extensionBase == "" {
		extensionBase = ""
		consoleBase = "/console/disaster-recovery"
	}
	setUIHeaders(writer, nonce)
	if err := backupTemplate.Execute(writer, map[string]string{"Nonce": nonce, "ExtensionBase": extensionBase, "ConsoleBase": consoleBase}); err != nil {
		http.Error(writer, fmt.Sprintf("render disaster recovery UI: %v", err), http.StatusInternalServerError)
	}
}

func uiNonce() (string, error) {
	data := make([]byte, 18)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(data), nil
}

func setUIHeaders(writer http.ResponseWriter, nonce string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Security-Policy", fmt.Sprintf("default-src 'none'; connect-src 'self'; script-src 'nonce-%s'; style-src 'nonce-%s'; frame-ancestors 'self'; img-src 'self'; form-action 'none'; base-uri 'none'", nonce, nonce))
}
