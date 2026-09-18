package protocol

import (
	"fmt"
	"html/template"
	"net/http"
)

var restoreTemplate = template.Must(template.New("restore").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Embedded Cluster recovery</title>
  <style nonce="{{.Nonce}}">
    body { margin: 0; background: #f5f7fb; color: #182230; font: 16px system-ui, sans-serif; }
    main { max-width: 760px; margin: 48px auto; background: white; padding: 32px; border-radius: 12px; box-shadow: 0 8px 30px #1d29391a; }
    h1 { margin-top: 0; } fieldset { border: 0; padding: 0; }
    label { display: block; margin: 16px 0 6px; font-weight: 600; }
    input, textarea { width: 100%; box-sizing: border-box; padding: 10px; border: 1px solid #cbd5e1; border-radius: 6px; }
    button { margin-top: 20px; padding: 10px 16px; border: 0; border-radius: 6px; background: #175cd3; color: white; font-weight: 650; cursor: pointer; }
    button.secondary { background: #475467; margin-left: 8px; }
    table { width: 100%; border-collapse: collapse; margin-top: 24px; }
    th, td { text-align: left; padding: 10px 6px; border-bottom: 1px solid #eaecf0; }
    .message { margin-top: 18px; padding: 12px; border-radius: 6px; background: #eff8ff; white-space: pre-wrap; }
  </style>
</head>
<body><main>
  <h1>Restore from a recovery point</h1>
  <p>The lifecycle extension connects directly to S3-compatible storage. Credentials and the recovery key are held only by this bootstrap process.</p>
  <fieldset>
    <label for="bucket">Bucket</label><input id="bucket" autocomplete="off">
    <label for="prefix">Prefix</label><input id="prefix" autocomplete="off">
    <label for="region">Region</label><input id="region" value="auto" autocomplete="off">
    <label for="endpoint">Endpoint (R2 or another S3-compatible store)</label><input id="endpoint" placeholder="https://ACCOUNT.r2.cloudflarestorage.com" autocomplete="off">
	<label for="proxyUrl">Proxy URL (optional)</label><input id="proxyUrl" placeholder="https://proxy.example.com" autocomplete="off">
	<label for="customCaPem">Custom CA certificate (optional)</label><textarea id="customCaPem" rows="5" autocomplete="off"></textarea>
    <label for="accessKeyId">Access key ID</label><input id="accessKeyId" autocomplete="off">
    <label for="secretAccessKey">Secret access key</label><input id="secretAccessKey" type="password" autocomplete="off">
    <label for="recoveryKey">Recovery key</label><input id="recoveryKey" type="password" autocomplete="off">
    <button id="loadPoints">Load recovery points</button>
  </fieldset>
  <div id="message" class="message">Enter storage details to list complete recovery points.</div>
  <table><thead><tr><th>Created</th><th>Recovery point</th><th></th></tr></thead><tbody id="points"></tbody></table>
<script nonce="{{.Nonce}}">
const operationID = {{.OperationID}};
function configuration() {
  return {storage: {
    bucket: document.getElementById('bucket').value,
    prefix: document.getElementById('prefix').value,
    region: document.getElementById('region').value,
    endpoint: document.getElementById('endpoint').value,
    accessKeyId: document.getElementById('accessKeyId').value,
    secretAccessKey: document.getElementById('secretAccessKey').value,
	forcePathStyle: true,
	proxyUrl: document.getElementById('proxyUrl').value,
	customCaPem: document.getElementById('customCaPem').value
  }, recoveryKey: document.getElementById('recoveryKey').value};
}
async function loadPoints() {
  message('Connecting to backup storage…');
  const response = await fetch('/ui/api/operations/' + operationID + '/recovery-points', {
    method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify({configuration: configuration()})
  });
  const body = await response.json();
  if (!response.ok) { message(body.message || 'Storage is unavailable.'); return; }
  const rows = document.getElementById('points'); rows.replaceChildren();
  for (const point of body.recoveryPoints) {
    const row = document.createElement('tr');
    const created = document.createElement('td'); created.textContent = new Date(point.createdAt).toLocaleString();
    const id = document.createElement('td'); id.textContent = point.id;
    const action = document.createElement('td'); const button = document.createElement('button'); button.className = 'secondary'; button.textContent = 'Restore';
    button.onclick = () => selectPoint(point.id); action.appendChild(button); row.append(created, id, action); rows.appendChild(row);
  }
  message(body.recoveryPoints.length ? 'Select a complete recovery point.' : 'No complete recovery points were found.');
}
async function selectPoint(id) {
  message('Preparing ' + id + '…');
  const response = await fetch('/ui/api/operations/' + operationID + '/select', {
    method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify({recoveryPointId: id, configuration: configuration()})
  });
  const body = await response.json();
  if (!response.ok) { message(body.message || 'Restore could not start.'); return; }
  document.getElementById('secretAccessKey').value = ''; document.getElementById('recoveryKey').value = '';
  message('Recovery point selected. Return to the installer terminal for progress.');
}
function message(value) { document.getElementById('message').textContent = value; }
document.getElementById('loadPoints').addEventListener('click', loadPoints);
</script>
</main></body></html>`))

func (s *Server) restoreUI(writer http.ResponseWriter, request *http.Request) {
	id := request.URL.Query().Get("operation")
	record := s.record(id)
	if record == nil || record.request.Operation != OperationRestore || record.request.Phase != PhaseBootstrap {
		http.Error(writer, "restore operation not found", http.StatusNotFound)
		return
	}
	nonce, err := uiNonce()
	if err != nil {
		http.Error(writer, "could not initialize restore UI", http.StatusInternalServerError)
		return
	}
	setUIHeaders(writer, nonce)
	if err := restoreTemplate.Execute(writer, map[string]string{"OperationID": id, "Nonce": nonce}); err != nil {
		http.Error(writer, fmt.Sprintf("render restore UI: %v", err), http.StatusInternalServerError)
	}
}
