package upload

// PageHTML is the upload page and the in-chat widget both: one document,
// self-contained, no external scripts, so the widget's content security
// policy needs to allow nothing but the connect back to this server. As a
// page it reads the token from its own URL; as a widget it asks for the link
// to be pasted, which is the same token by another route.
const PageHTML = `<!doctype html>
<html lang="fi">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Prodeko – kuvan lataus</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 28rem; margin: 3rem auto; padding: 0 1rem; color: #1a2340; }
  h1 { font-size: 1.2rem; }
  #drop { border: 2px dashed #8892b0; border-radius: 8px; padding: 2.5rem 1rem; text-align: center; cursor: pointer; }
  #drop.hot { border-color: #123274; background: #f0f4ff; }
  #out { margin-top: 1rem; white-space: pre-wrap; word-break: break-all; }
  .ok { color: #1a6b3c; } .err { color: #a4243b; }
  input[type=file] { display: none; }
  #tokenrow { margin-top: 1rem; font-size: .85rem; }
  #tokenrow input { width: 100%; }
</style>
<h1>Lataa kuva prodeko.org-muutokseen</h1>
<p>JPEG tai PNG, enintään 5 Mt. Linkki toimii kerran ja vanhenee 15 minuutissa.</p>
<div id="drop">Pudota kuva tähän tai napauta valitaksesi<input id="file" type="file" accept="image/jpeg,image/png"></div>
<div id="tokenrow" hidden>
  <label>Liitä latauslinkki keskustelusta: <input id="link" placeholder="https://mcp.prodeko.org/upload?token=..."></label>
</div>
<p id="out"></p>
<script>
  var drop = document.getElementById('drop'), file = document.getElementById('file'),
      out = document.getElementById('out'), linkRow = document.getElementById('tokenrow'),
      link = document.getElementById('link');

  function token() {
    var q = new URLSearchParams(location.search).get('token');
    if (q) return q;
    try { return new URL(link.value).searchParams.get('token') || ''; } catch (e) { return ''; }
  }
  // Inside the widget iframe there is no query string, so the pasted link is
  // the way the token arrives there.
  if (!new URLSearchParams(location.search).get('token')) linkRow.hidden = false;

  function send(f) {
    if (!f) return;
    var t = token();
    if (!t) { out.textContent = 'Latauslinkin token puuttuu – liitä linkki yllä olevaan kenttään.'; out.className = 'err'; return; }
    out.textContent = 'Ladataan…'; out.className = '';
    var fd = new FormData();
    fd.append('token', t);
    fd.append('file', f, f.name);
    fetch(location.pathname, { method: 'POST', body: fd })
      .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, j: j }; }); })
      .then(function (r) {
        if (r.ok) { out.textContent = 'Valmis: ' + r.j.path + '\n' + r.j.next; out.className = 'ok'; }
        else { out.textContent = r.j.error || 'Lataus epäonnistui.'; out.className = 'err'; }
      })
      .catch(function () { out.textContent = 'Lataus epäonnistui – yhteysvirhe.'; out.className = 'err'; });
  }
  drop.addEventListener('click', function () { file.click(); });
  file.addEventListener('change', function () { send(file.files[0]); });
  drop.addEventListener('dragover', function (e) { e.preventDefault(); drop.className = 'hot'; });
  drop.addEventListener('dragleave', function () { drop.className = ''; });
  drop.addEventListener('drop', function (e) {
    e.preventDefault(); drop.className = '';
    send(e.dataTransfer.files[0]);
  });
</script>
</html>
`
