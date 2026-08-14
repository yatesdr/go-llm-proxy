package handler

import (
	"log/slog"
	"net/http"
	"net/url"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

// PoolsPage renders the /admin/pools HTML page.
func (h *AdminHandler) PoolsPage(w http.ResponseWriter, r *http.Request) {
	body := `<div class="toolbar">
  <h2>Backend Pools</h2>
  <button class="btn btn-primary btn-sm" type="button" onclick="openPoolModal(null)">+ Add Pool</button>
</div>
<p class="pool-intro">A pool groups interchangeable backends serving the same model. Point a model at a pool
(Models tab &rarr; Backend source) and the proxy spreads sessions across the pool's backends.</p>
<div id="poolsList"><div class="card" style="text-align:center;color:var(--muted)">Loading…</div></div>` + poolModalHTML()
	h.renderShell(w, "pools", "Admin · Pools", body, poolsPageJS())
}

// PoolsData serves the JSON payload the /admin/pools page fetches.
func (h *AdminHandler) PoolsData(w http.ResponseWriter, r *http.Request) {
	cfg := h.cs.Get()

	pools := make([]map[string]any, 0, len(cfg.Pools))
	for _, p := range cfg.Pools {
		backends := make([]map[string]any, 0, len(p.Backends))
		for _, b := range p.Backends {
			backends = append(backends, map[string]any{
				"url":          b.URL,
				"has_api_key":  b.APIKey != "",
				"api_key_mask": config.MaskSecret(b.APIKey),
				"weight":       b.Weight,
				"max_inflight": b.MaxInflight,
				"disabled":     b.Disabled,
			})
		}
		pools = append(pools, map[string]any{
			"name":          p.Name,
			"backends":      backends,
			"referenced_by": config.PoolReferrers(cfg, p.Name),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"pools": pools})
}

// poolBackendDTO carries one backend row from the pool editor. APIKey is
// pointer-typed: nil = keep the existing key for this URL, "" = clear,
// non-empty = replace.
type poolBackendDTO struct {
	URL         string  `json:"url"`
	APIKey      *string `json:"api_key"`
	Weight      int     `json:"weight"`
	MaxInflight int     `json:"max_inflight"`
	Disabled    bool    `json:"disabled"`
}

// PoolsMutate handles POST /admin/pools/mutate.
func (h *AdminHandler) PoolsMutate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action       string           `json:"action"`
		OriginalName string           `json:"original_name"`
		Name         string           `json:"name"`
		Backends     []poolBackendDTO `json:"backends"`
	}
	if err := decodeJSONBody(r, &req, 64*1024); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	switch req.Action {
	case "add":
		p := config.PoolConfig{Name: req.Name, Backends: h.resolvePoolBackends(nil, req.Backends)}
		if err := h.cs.AddPool(p); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: pool added", "name", p.Name, "backends", len(p.Backends))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": p.Name})

	case "update":
		if req.OriginalName == "" {
			httputil.WriteError(w, http.StatusBadRequest, "original_name is required")
			return
		}
		existing := config.FindPool(h.cs.Get(), req.OriginalName)
		if existing == nil {
			httputil.WriteError(w, http.StatusNotFound, "pool not found")
			return
		}
		p := config.PoolConfig{Name: req.Name, Backends: h.resolvePoolBackends(existing, req.Backends)}
		if err := h.cs.UpdatePool(req.OriginalName, p); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: pool updated", "original", req.OriginalName, "name", p.Name, "backends", len(p.Backends))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": p.Name})

	case "delete":
		if req.Name == "" {
			httputil.WriteError(w, http.StatusBadRequest, "name is required")
			return
		}
		if err := h.cs.DeletePool(req.Name); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: pool deleted", "name", req.Name)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		httputil.WriteError(w, http.StatusBadRequest, "unknown action")
	}
}

// resolvePoolBackends converts DTO rows to config entries, preserving existing
// API keys for rows whose api_key was omitted (matched by URL in the pool
// being edited; nil existing pool = add case, omitted keys resolve empty).
func (h *AdminHandler) resolvePoolBackends(existing *config.PoolConfig, rows []poolBackendDTO) []config.BackendConfig {
	currentKey := func(url string) string {
		if existing == nil {
			return ""
		}
		for _, b := range existing.Backends {
			if b.URL == url {
				return b.APIKey
			}
		}
		return ""
	}
	out := make([]config.BackendConfig, 0, len(rows))
	for _, row := range rows {
		b := config.BackendConfig{
			URL:         row.URL,
			Weight:      row.Weight,
			MaxInflight: row.MaxInflight,
			Disabled:    row.Disabled,
		}
		if row.APIKey != nil {
			b.APIKey = *row.APIKey
		} else {
			b.APIKey = currentKey(row.URL)
		}
		out = append(out, b)
	}
	return out
}

// PoolsProbe handles POST /admin/pools/probe: a one-shot reachability and
// engine-detection check for a backend URL being added to a pool. The api_key
// field may name an existing pool backend ("pool:URL" omitted → key looked up)
// or carry a literal key.
func (h *AdminHandler) PoolsProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL    string  `json:"url"`
		APIKey *string `json:"api_key"` // nil = use the stored key for this URL, if any
		Pool   string  `json:"pool"`    // pool to look the stored key up in (optional)
		Type   string  `json:"type"`
	}
	if err := decodeJSONBody(r, &req, 8*1024); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.URL == "" {
		httputil.WriteError(w, http.StatusBadRequest, "url is required")
		return
	}
	if u, err := url.Parse(req.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		httputil.WriteError(w, http.StatusBadRequest, "url must be a valid http(s) URL")
		return
	}

	apiKey := ""
	if req.APIKey != nil {
		apiKey = *req.APIKey
	} else if req.Pool != "" {
		if p := config.FindPool(h.cs.Get(), req.Pool); p != nil {
			for _, b := range p.Backends {
				if b.URL == req.URL {
					apiKey = b.APIKey
					break
				}
			}
		}
	}

	res := config.ProbeBackend(req.URL, apiKey, req.Type)
	writeJSON(w, http.StatusOK, res)
}

// poolModalHTML returns the hidden modal shell used for both Add and Edit.
func poolModalHTML() string {
	return `<div id="poolModal" class="modal-backdrop" onclick="onPoolBackdropClick(event)">
  <div class="modal" role="dialog" aria-labelledby="poolModalTitle" style="max-width:760px">
    <div class="modal-header">
      <h2 id="poolModalTitle">Pool</h2>
      <button class="modal-close" type="button" onclick="closePoolModal()" aria-label="Close">&times;</button>
    </div>
    <form id="poolForm" onsubmit="submitPool(event)">
      <div class="modal-body">
        <div class="section section-required"><h3>Pool</h3>
          <div class="field">
            <label>Name <span class="tip" tabindex="0" data-tip="Identifier models use to reference this pool (e.g. 'glm-cluster'). Renaming updates every model that references it.">?</span></label>
            <input type="text" name="pool_name" required placeholder="e.g. glm-cluster">
          </div>
        </div>
        <div class="section"><h3>Backends</h3>
          <div id="poolBackendRows"></div>
          <button type="button" class="btn btn-secondary btn-sm" onclick="addBackendRow(null)">+ Add Backend</button>
        </div>
        <div id="poolFormErr" class="inline-err" style="display:none"></div>
      </div>
      <div class="modal-footer">
        <button type="button" class="btn btn-secondary" onclick="closePoolModal()">Cancel</button>
        <button type="submit" class="btn btn-primary" id="poolSaveBtn">Save</button>
      </div>
    </form>
  </div>
</div>`
}

// poolsPageJS returns the JavaScript for the pools tab.
func poolsPageJS() string {
	return `
var pstate = {pools: [], editing: null, rows: []};

function loadPools(){
  apiGet("/admin/pools/data").then(function(d){
    pstate.pools = d.pools || [];
    renderPools();
  }).catch(function(e){ flash("Load failed: "+e.message, "error"); });
}

function renderPools(){
  var el = document.getElementById("poolsList");
  if(!pstate.pools.length){
    el.innerHTML = '<div class="card" style="text-align:center;color:var(--muted)">No pools defined. Add one to start load-balancing a model across multiple backends.</div>';
    return;
  }
  var html = "";
  for(var i=0;i<pstate.pools.length;i++){
    var p = pstate.pools[i];
    var refs = (p.referenced_by || []);
    var refHTML = refs.length
      ? refs.map(function(m){ return '<span class="pill">'+esc(m)+'</span>'; }).join('')
      : '<span class="pill pill-unrestricted">no models attached</span>';
    var rows = "";
    for(var j=0;j<p.backends.length;j++){
      var b = p.backends[j];
      rows += '<tr'+(b.disabled?' class="backend-disabled"':'')+'>' +
        '<td class="mono" title="'+esc(b.url)+'">'+esc(b.url)+'</td>' +
        '<td class="mono">'+(b.has_api_key ? esc(b.api_key_mask) : "—")+'</td>' +
        '<td style="text-align:center">'+esc(b.weight || 1)+'</td>' +
        '<td style="text-align:center">'+(b.max_inflight ? esc(b.max_inflight) : "∞")+'</td>' +
        '<td style="text-align:center">'+(b.disabled ? '<span class="drain-badge">drained</span>' : '<span class="health-dot health-unknown" id="probe-dot-'+i+'-'+j+'"></span><span class="mono" id="probe-label-'+i+'-'+j+'">—</span>')+'</td>' +
        '<td class="row-actions"><button class="btn btn-secondary btn-sm" onclick="probeRow('+i+','+j+')">Probe</button></td>' +
      '</tr>';
    }
    html += '<div class="card" style="margin-bottom:16px">' +
      '<div class="toolbar" style="margin-bottom:8px"><h2 style="font-size:1.05rem">'+esc(p.name)+'</h2>' +
        '<div class="action-group" style="display:inline-flex;gap:6px">' +
        '<button class="btn btn-secondary btn-sm" onclick="openPoolModal(\''+escAttr(p.name)+'\')">Edit</button>' +
        '<button class="btn btn-danger btn-sm" onclick="deletePool(\''+escAttr(p.name)+'\')">Delete</button>' +
        '</div></div>' +
      '<div style="margin-bottom:10px">'+refHTML+'</div>' +
      '<div class="table-wrap"><table class="data-table"><thead><tr>' +
        '<th>Backend URL</th><th style="width:120px">API key</th><th style="width:70px;text-align:center">Weight</th><th style="width:100px;text-align:center">Max in-flight</th><th style="width:130px;text-align:center">Status</th><th style="width:80px;text-align:right"></th>' +
      '</tr></thead><tbody>'+rows+'</tbody></table></div>' +
    '</div>';
  }
  el.innerHTML = html;
}

function probeRow(pi, bi){
  var p = pstate.pools[pi], b = p.backends[bi];
  var dot = document.getElementById("probe-dot-"+pi+"-"+bi);
  var label = document.getElementById("probe-label-"+pi+"-"+bi);
  if(label) label.textContent = "…";
  apiPost("/admin/pools/probe", {url: b.url, pool: p.name}).then(function(res){
    var r = res.json || {};
    if(!dot || !label) return;
    if(r.reachable){
      dot.className = "health-dot health-online";
      label.textContent = r.engine ? r.engine + (r.context_window ? " · " + Number(r.context_window).toLocaleString() : "") : "online";
    } else {
      dot.className = "health-dot health-offline";
      label.textContent = "offline";
      dot.title = r.error || "";
      flash("Probe failed: " + (r.error || "unreachable"), "error");
    }
  }).catch(function(e){ if(label) label.textContent = "error"; flash("Probe failed: "+e.message, "error"); });
}

// ── Pool modal ──────────────────────────────────────────────────────────────

function openPoolModal(name){
  var p = name ? pstate.pools.find(function(x){return x.name === name;}) : null;
  pstate.editing = name;
  pstate.rows = [];
  document.getElementById("poolModalTitle").textContent = name ? ("Edit: "+name) : "Add Pool";
  document.getElementById("poolForm").reset();
  document.getElementById("poolForm").elements["pool_name"].value = name || "";
  document.getElementById("poolBackendRows").innerHTML = "";
  if(p && p.backends.length){
    for(var i=0;i<p.backends.length;i++) addBackendRow(p.backends[i]);
  } else {
    addBackendRow(null);
  }
  document.getElementById("poolFormErr").style.display = "none";
  document.getElementById("poolModal").classList.add("open");
  setTimeout(function(){ document.getElementById("poolForm").elements["pool_name"].focus(); }, 20);
}

function closePoolModal(){
  document.getElementById("poolModal").classList.remove("open");
  pstate.editing = null;
}

function onPoolBackdropClick(ev){
  if(ev.target.id === "poolModal") closePoolModal();
}

document.addEventListener("keydown", function(ev){
  if(ev.key === "Escape" && document.getElementById("poolModal").classList.contains("open")){
    closePoolModal();
  }
});

// Each row: {id, existing: bool, has_api_key, api_key_mask, keyState: "keep"|"clear"|"set"}
var rowSeq = 0;
function addBackendRow(b){
  var id = "brow" + (rowSeq++);
  pstate.rows.push({id: id, existing: !!b, has_api_key: b ? b.has_api_key : false,
                    api_key_mask: b ? b.api_key_mask : "", keyState: b ? "keep" : "set"});
  var wrap = document.createElement("div");
  wrap.className = "backend-row";
  wrap.id = id;
  wrap.innerHTML =
    '<div class="field-grid" style="grid-template-columns:2fr 1fr;margin-bottom:4px">' +
      '<div class="field"><label>URL</label><input type="url" data-f="url" placeholder="http://10.0.0.10:8000/v1" required></div>' +
      '<div class="field"><label>API key</label><div class="secret-row">' +
        '<span class="mono" data-f="keymask">(not set)</span>' +
        '<button type="button" class="btn btn-secondary btn-sm" onclick="rowKeyRotate(\''+id+'\')">Set</button>' +
        '<button type="button" class="btn btn-danger btn-sm" onclick="rowKeyClear(\''+id+'\')">Clear</button>' +
      '</div><input type="password" data-f="api_key" style="display:none;margin-top:4px" placeholder="key sent to this backend"></div>' +
    '</div>' +
    '<div class="field-grid" style="grid-template-columns:1fr 1fr 1fr auto;align-items:end">' +
      '<div class="field"><label>Weight</label><input type="number" data-f="weight" min="1" step="1" placeholder="1"></div>' +
      '<div class="field"><label>Max in-flight</label><input type="number" data-f="max_inflight" min="0" step="1" placeholder="unlimited"></div>' +
      '<div class="field checkbox-row" style="padding-top:0"><input type="checkbox" data-f="disabled" id="'+id+'-dis"><label for="'+id+'-dis">Drained <span class="tip" tabindex="0" data-tip="Excluded from load balancing without being deleted. Use to take a backend down for maintenance.">?</span></label></div>' +
      '<div class="field"><button type="button" class="btn btn-danger btn-sm" onclick="removeBackendRow(\''+id+'\')">Remove</button></div>' +
    '</div><hr style="border:none;border-top:1px solid var(--border);margin:10px 0">';
  document.getElementById("poolBackendRows").appendChild(wrap);
  if(b){
    wrap.querySelector('[data-f=url]').value = b.url;
    wrap.querySelector('[data-f=keymask]').textContent = b.has_api_key ? b.api_key_mask : "(not set)";
    if(b.weight && b.weight !== 1) wrap.querySelector('[data-f=weight]').value = b.weight;
    if(b.max_inflight) wrap.querySelector('[data-f=max_inflight]').value = b.max_inflight;
    wrap.querySelector('[data-f=disabled]').checked = !!b.disabled;
  } else {
    // New row: key input visible immediately (nothing stored to keep).
    var inp = wrap.querySelector('[data-f=api_key]');
    inp.style.display = "block";
    wrap.querySelector('[data-f=keymask]').textContent = "(not set)";
  }
}

function rowState(id){ return pstate.rows.find(function(r){return r.id === id;}); }

function rowKeyRotate(id){
  var st = rowState(id); if(st) st.keyState = "set";
  var wrap = document.getElementById(id);
  var inp = wrap.querySelector('[data-f=api_key]');
  inp.style.display = "block"; inp.focus();
}

function rowKeyClear(id){
  var st = rowState(id); if(st) st.keyState = "clear";
  var wrap = document.getElementById(id);
  wrap.querySelector('[data-f=keymask]').textContent = "(will be cleared on save)";
  var inp = wrap.querySelector('[data-f=api_key]');
  inp.style.display = "none"; inp.value = "";
}

function removeBackendRow(id){
  var el = document.getElementById(id);
  if(el) el.remove();
  pstate.rows = pstate.rows.filter(function(r){return r.id !== id;});
}

function collectPool(){
  var name = document.getElementById("poolForm").elements["pool_name"].value.trim();
  var backends = [];
  for(var i=0;i<pstate.rows.length;i++){
    var st = pstate.rows[i];
    var wrap = document.getElementById(st.id);
    if(!wrap) continue;
    var url = wrap.querySelector('[data-f=url]').value.trim();
    if(!url) continue;
    var row = {
      url: url,
      weight: parseInt(wrap.querySelector('[data-f=weight]').value,10) || 0,
      max_inflight: parseInt(wrap.querySelector('[data-f=max_inflight]').value,10) || 0,
      disabled: wrap.querySelector('[data-f=disabled]').checked
    };
    // API key semantics mirror the model modal: "clear" sends "", "set" sends
    // the typed value only when non-empty, otherwise the key is omitted (keep).
    if(st.keyState === "clear"){
      row.api_key = "";
    } else if(st.keyState === "set"){
      var v = wrap.querySelector('[data-f=api_key]').value.trim();
      if(v !== "") row.api_key = v;
      else if(!st.existing) row.api_key = "";
    }
    backends.push(row);
  }
  return {name: name, backends: backends};
}

function submitPool(ev){
  ev.preventDefault();
  var errEl = document.getElementById("poolFormErr");
  errEl.style.display = "none";
  var body = collectPool();
  if(!body.name){ return showPoolErr("Pool name is required"); }
  if(!body.backends.length){ return showPoolErr("At least one backend is required"); }
  for(var i=0;i<body.backends.length;i++){
    try { new URL(body.backends[i].url); } catch(e){ return showPoolErr("Backend URL is invalid: "+body.backends[i].url); }
  }
  var btn = document.getElementById("poolSaveBtn");
  btn.disabled = true; btn.textContent = "Saving…";
  var req;
  if(pstate.editing){
    req = apiPost("/admin/pools/mutate", {action:"update", original_name: pstate.editing, name: body.name, backends: body.backends});
  } else {
    req = apiPost("/admin/pools/mutate", {action:"add", name: body.name, backends: body.backends});
  }
  req.then(function(res){
    btn.disabled = false; btn.textContent = "Save";
    if(!res.ok){ showPoolErr(res.json.error && res.json.error.message || "Save failed"); return; }
    closePoolModal();
    flash("Saved", "success");
    loadPools();
  }).catch(function(e){
    btn.disabled = false; btn.textContent = "Save";
    showPoolErr(e.message || "Save failed");
  });
}

function showPoolErr(msg){
  var el = document.getElementById("poolFormErr");
  el.textContent = msg;
  el.style.display = "block";
}

function deletePool(name){
  if(!confirm('Delete pool "'+name+'"? Models referencing it will block the delete.')) return;
  apiPost("/admin/pools/mutate", {action:"delete", name: name}).then(function(res){
    if(!res.ok){ flash((res.json.error && res.json.error.message) || "Delete failed", "error"); return; }
    flash("Deleted", "success");
    loadPools();
  });
}

loadPools();
`
}
