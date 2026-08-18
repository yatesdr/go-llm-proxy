package handler

import (
	"log/slog"
	"net/http"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/usage"
)

// UsersPage renders the /admin/users HTML page.
func (h *AdminHandler) UsersPage(w http.ResponseWriter, r *http.Request) {
	body := `<div class="card">
  <div class="card-header">
    <h2>API Keys</h2>
    <div class="card-tools">
      <input id="userFilter" class="filter-input" type="search" placeholder="Filter by name, key, or model&hellip;" autocomplete="off">
      <button class="btn btn-primary" type="button" onclick="openUserModal('add')">+ Add Key</button>
    </div>
  </div>
  <div class="table-wrap">
    <table class="data-table">
      <thead><tr><th style="width:170px">Name</th><th style="width:140px">Key</th><th>Allowed Models</th><th style="width:96px" title="Most recent request in the last 30 days">Last seen</th><th style="width:88px;text-align:right" title="Requests in the last 30 days">Req · 30d</th><th style="width:112px;text-align:right" title="Tokens in the last 30 days">Tokens · 30d</th><th style="width:170px;text-align:right">Actions</th></tr></thead>
      <tbody id="usersBody"><tr><td colspan="7" style="text-align:center;color:var(--muted)">Loading…</td></tr></tbody>
    </table>
  </div>
</div>
<div id="userModal" class="modal-backdrop" onclick="if(event.target.id==='userModal')closeUserModal()">
  <div class="modal" style="max-width:400px" role="dialog">
    <div class="modal-header"><h2 id="userModalTitle">Add API key</h2><button class="modal-close" type="button" onclick="closeUserModal()">&times;</button></div>
    <form onsubmit="submitUserModal(event)">
      <div class="modal-body">
        <div class="field"><label for="userNameInput">Name</label><input type="text" id="userNameInput" required maxlength="100" autocomplete="off" placeholder="who this key belongs to"></div>
        <p class="hint" id="userModalHint"></p>
        <div id="userModalErr" class="inline-err" style="display:none"></div>
      </div>
      <div class="modal-footer"><button type="button" class="btn btn-secondary" onclick="closeUserModal()">Cancel</button><button id="userModalSave" class="btn btn-primary" type="submit">Create key</button></div>
    </form>
  </div>
</div>
<div id="keyModal" class="modal-backdrop" onclick="if(event.target.id==='keyModal')closeKeyModal()">
  <div class="modal" style="max-width:560px" role="dialog">
    <div class="modal-header"><h2 id="keyModalTitle">API key</h2><button class="modal-close" type="button" onclick="closeKeyModal()">&times;</button></div>
    <div class="modal-body">
      <div class="field"><label>Name</label><div id="keyModalName" style="font-weight:600"></div></div>
      <div class="field"><label>LLM Key</label>
        <div class="secret-row" style="align-items:stretch">
          <code id="keyModalVal" style="flex:1;display:block;background:var(--panel);border:1px solid var(--hairline);padding:8px 10px;font-family:var(--font-mono);font-size:12px;word-break:break-all"></code>
          <button class="btn btn-secondary" type="button" onclick="copyKeyModal()">Copy</button>
        </div>
      </div>
      <p class="hint" id="keyModalHint" style="display:none"></p>
    </div>
    <div class="modal-footer"><button class="btn btn-secondary" type="button" onclick="closeKeyModal()">Close</button></div>
  </div>
</div>
`
	script := usersPageJS()
	h.renderShell(w, "users", "Admin · API Keys", body, script)
}

// UsersData serves the JSON payload the /admin/users page fetches on load.
func (h *AdminHandler) UsersData(w http.ResponseWriter, r *http.Request) {
	cfg := h.cs.Get()

	var activity map[string]usage.KeyActivity
	if h.ul != nil {
		activity, _ = h.ul.QueryKeyActivity(30) // best-effort; nil map on error
	}

	users := make([]map[string]any, 0, len(cfg.Keys))
	for _, k := range cfg.Keys {
		models := k.Models
		if models == nil {
			models = []string{}
		}
		hash := config.KeyHash(k.Key)
		entry := map[string]any{
			"name":     k.Name,
			"key_hash": hash,
			"masked":   config.MaskKey(k.Key),
			"models":   models,
		}
		if a, ok := activity[hash]; ok {
			entry["last_seen"] = a.LastSeen
			entry["requests_30d"] = a.Requests
			entry["tokens_30d"] = a.TotalTokens
		}
		users = append(users, entry)
	}

	allModels := make([]string, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		allModels = append(allModels, m.Name)
	}
	if cfg.Audio.Whisper != nil {
		allModels = append(allModels, cfg.Audio.Whisper.Name)
	}
	if cfg.Audio.TTS != nil {
		allModels = append(allModels, cfg.Audio.TTS.Name)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"users":      users,
		"all_models": allModels,
	})
}

// UsersMutate handles POST /admin/users/mutate.
func (h *AdminHandler) UsersMutate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action  string   `json:"action"`
		KeyHash string   `json:"key_hash"`
		Name    string   `json:"name"`
		Models  []string `json:"models"`
	}
	if err := decodeJSONBody(r, &req, 32*1024); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	switch req.Action {
	case "add":
		key, err := h.cs.AddKey(req.Name)
		if err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: api key added", "name", req.Name, "hash", config.KeyHash(key)[:8])
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"key":      key, // full key, shown once
			"key_hash": config.KeyHash(key),
			"masked":   config.MaskKey(key),
			"name":     req.Name,
		})
	case "update_models":
		if req.KeyHash == "" {
			httputil.WriteError(w, http.StatusBadRequest, "key_hash is required")
			return
		}
		if err := h.cs.UpdateKeyModels(req.KeyHash, req.Models); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: api key models updated", "hash", req.KeyHash[:min(8, len(req.KeyHash))], "models", req.Models)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "rename":
		if req.KeyHash == "" {
			httputil.WriteError(w, http.StatusBadRequest, "key_hash is required")
			return
		}
		if err := h.cs.RenameKey(req.KeyHash, req.Name); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: api key renamed", "hash", req.KeyHash[:min(8, len(req.KeyHash))], "new_name", req.Name)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "reveal":
		// Recovery path: return the full key for an existing entry. Keys are
		// stored in plaintext in the config, so this grants no privilege the
		// admin doesn't already hold — but each reveal is audit-logged.
		if req.KeyHash == "" {
			httputil.WriteError(w, http.StatusBadRequest, "key_hash is required")
			return
		}
		fullKey := config.LookupKeyByHash(h.cs.Get(), req.KeyHash)
		if fullKey == "" {
			httputil.WriteError(w, http.StatusNotFound, "key not found")
			return
		}
		slog.Info("admin: api key revealed", "hash", req.KeyHash[:min(8, len(req.KeyHash))])
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": fullKey})

	case "rotate":
		// Reissue path: replace the secret, keep name + model allowlist.
		// The old key stops working immediately.
		if req.KeyHash == "" {
			httputil.WriteError(w, http.StatusBadRequest, "key_hash is required")
			return
		}
		newKey, err := h.cs.RotateKey(req.KeyHash)
		if err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: api key rotated",
			"old_hash", req.KeyHash[:min(8, len(req.KeyHash))],
			"new_hash", config.KeyHash(newKey)[:8])
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"key":      newKey,
			"key_hash": config.KeyHash(newKey),
			"masked":   config.MaskKey(newKey),
		})

	case "delete":
		if req.KeyHash == "" {
			httputil.WriteError(w, http.StatusBadRequest, "key_hash is required")
			return
		}
		if err := h.cs.DeleteKey(req.KeyHash); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: api key deleted", "hash", req.KeyHash[:min(8, len(req.KeyHash))])
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		httputil.WriteError(w, http.StatusBadRequest, "unknown action")
	}
}

// writeMutateError translates common mutator errors into appropriate HTTP
// statuses.
func writeMutateError(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case contains(msg, "not found"):
		httputil.WriteError(w, http.StatusNotFound, msg)
	case contains(msg, "already exists"), contains(msg, "last remaining"), contains(msg, "referenced by"):
		httputil.WriteError(w, http.StatusConflict, msg)
	case contains(msg, "invalid"), contains(msg, "required"), contains(msg, "unknown"), contains(msg, "too long"):
		httputil.WriteError(w, http.StatusBadRequest, msg)
	default:
		httputil.WriteError(w, http.StatusInternalServerError, msg)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	if len(sub) > len(s) {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func usersPageJS() string {
	return `
var state = {users: [], allModels: []};

function load(){
  apiGet("/admin/users/data").then(function(d){
    state.users = d.users || [];
    state.allModels = d.all_models || [];
    renderUsers();
  }).catch(function(e){ flash("Load failed: "+e.message, "error"); });
}

function renderUsers(){
  var tbody = document.getElementById("usersBody");
  if(!state.users.length){
    tbody.innerHTML = '<tr><td colspan="7" style="text-align:center;color:var(--muted)">No API keys configured — add one to grant access</td></tr>';
    return;
  }
  var html = "";
  for(var i=0;i<state.users.length;i++){
    html += renderUserRow(state.users[i]);
  }
  tbody.innerHTML = html;
  reapplyFilter("userFilter", "usersBody");
}

function activityCells(u){
  if(u.last_seen){
    return '<td class="mono">'+esc(u.last_seen)+'</td>'+
      '<td class="mono" style="text-align:right">'+Number(u.requests_30d||0).toLocaleString()+'</td>'+
      '<td class="mono" style="text-align:right">'+Number(u.tokens_30d||0).toLocaleString()+'</td>';
  }
  return '<td class="mono" style="color:var(--faint)">&mdash;</td>'+
    '<td class="mono" style="text-align:right;color:var(--faint)">0</td>'+
    '<td class="mono" style="text-align:right;color:var(--faint)">0</td>';
}

function renderUserRow(u){
  var pills = "";
  if(!u.models || u.models.length === 0){
    pills = '<span class="pill pill-unrestricted" title="No restrictions — key can access every configured model">all models</span>';
  } else {
    for(var i=0;i<u.models.length;i++){
      pills += '<span class="pill">'+esc(u.models[i])+
        '<button class="pill-x" title="Remove access" onclick="removeModel(\''+u.key_hash+'\',\''+escAttr(u.models[i])+'\')">&times;</button></span>';
    }
  }
  pills += '<button class="pill-add" onclick="showAddModel(\''+u.key_hash+'\', this)">+ add model…</button>';
  return '<tr data-hash="'+u.key_hash+'">' +
    '<td class="cell-name"><span class="editable" title="Click to rename" onclick="editName(this, \''+u.key_hash+'\')">'+esc(u.name)+'</span></td>' +
    '<td class="cell-key"><code>'+esc(u.masked)+'</code></td>' +
    '<td class="cell-pills"><div class="pills-wrap">'+pills+'</div></td>' +
    activityCells(u) +
    '<td class="row-actions"><div class="action-group">'+
      '<button class="btn btn-secondary btn-sm" onclick="revealKey(\''+u.key_hash+'\', \''+escAttr(u.name)+'\')" title="Show the full key in a banner">Reveal</button>'+
      '<button class="btn btn-secondary btn-sm btn-icon" onclick="confirmRotate(\''+u.key_hash+'\')" title="Rotate key — replaces it with a new one; the old key stops working immediately"><svg viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M13.5 8A5.5 5.5 0 1 1 11.7 3.9"/><path d="M13.9 1.6v3h-3"/></svg></button>'+
      '<button class="btn btn-danger btn-sm btn-icon" onclick="confirmDelete(\''+u.key_hash+'\')" title="Delete key — revokes access immediately"><svg viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><path d="M2.5 4h11M5.5 4V2.5h5V4M4 4l.7 9.5h6.6L12 4M6.5 6.8v4.4M9.5 6.8v4.4"/></svg></button>'+
    '</div></td></tr>';
}

// ---- Inline rename ----
function userByHash(h){ return state.users.find(function(u){return u.key_hash===h;}); }

function editName(el, hash){
  var u = userByHash(hash); if(!u) return;
  var input = document.createElement("input");
  input.type = "text"; input.className = "editable-input";
  input.value = u.name; input.maxLength = 100;
  el.replaceWith(input); input.focus(); input.select();
  var done = false;
  function finish(save){
    if(done) return; done = true;
    var val = input.value.trim();
    if(save && val && val !== u.name){
      apiPost("/admin/users/mutate", {action:"rename", key_hash: hash, name: val}).then(function(res){
        if(!res.ok){ flash(res.json.error && res.json.error.message || "Rename failed", "error"); }
        load();
      });
    } else { load(); }
  }
  input.addEventListener("keydown", function(ev){
    if(ev.key === "Enter"){ ev.preventDefault(); finish(true); }
    else if(ev.key === "Escape"){ finish(false); }
  });
  input.addEventListener("blur", function(){ finish(true); });
}

function activitySummary(u){
  if(u && u.last_seen){
    return 'Last used '+u.last_seen+' · '+Number(u.requests_30d||0).toLocaleString()+' requests in the last 30 days.';
  }
  return 'No recorded activity in the last 30 days.';
}

var modalState = {mode:null, hash:null};

function openUserModal(mode, hash, currentName){
  modalState.mode = mode; modalState.hash = hash||null;
  var input = document.getElementById("userNameInput");
  document.getElementById("userModalErr").style.display = "none";
  if(mode === "add"){
    document.getElementById("userModalTitle").textContent = "Add API key";
    document.getElementById("userModalHint").textContent = "A new API key is generated and shown once. It stays recoverable via Reveal.";
    document.getElementById("userModalSave").textContent = "Create key";
    input.value = "";
  } else {
    document.getElementById("userModalTitle").textContent = "Rename user";
    document.getElementById("userModalHint").textContent = "Renaming does not change the key itself.";
    document.getElementById("userModalSave").textContent = "Rename";
    input.value = currentName || "";
  }
  document.getElementById("userModal").classList.add("open");
  input.focus(); input.select();
}
function closeUserModal(){ document.getElementById("userModal").classList.remove("open"); }

function submitUserModal(ev){
  ev.preventDefault();
  var name = document.getElementById("userNameInput").value.trim();
  if(!name) return;
  var err = document.getElementById("userModalErr");
  var body = modalState.mode === "add"
    ? {action:"add", name:name}
    : {action:"rename", key_hash: modalState.hash, name:name};
  apiPost("/admin/users/mutate", body).then(function(res){
    if(!res.ok){
      err.textContent = res.json.error && res.json.error.message || "Save failed";
      err.style.display = "block";
      return;
    }
    closeUserModal();
    if(modalState.mode === "add") showKeyModal(res.json.name, res.json.key, "new");
    load();
  });
}

document.addEventListener("keydown", function(e){
  if(e.key === "Escape"){ closeUserModal(); closeKeyModal(); }
});

function showKeyModal(name, key, mode){
  document.getElementById("keyModalTitle").textContent = mode === "new" ? "New API key" : (mode === "rotated" ? "Key rotated" : "API key");
  document.getElementById("keyModalName").textContent = name;
  document.getElementById("keyModalVal").textContent = key;
  var hint = document.getElementById("keyModalHint");
  if(mode === "rotated"){ hint.textContent = "The old key is now invalid — anything still using it gets 401s until updated."; hint.style.display = ""; }
  else if(mode === "new"){ hint.textContent = "Recoverable later via Reveal."; hint.style.display = ""; }
  else { hint.style.display = "none"; }
  document.getElementById("keyModal").classList.add("open");
}
function closeKeyModal(){ document.getElementById("keyModal").classList.remove("open"); }
function copyKeyModal(){
  copyToClipboard(document.getElementById("keyModalVal").textContent).then(function(){ flash("Copied to clipboard", "success"); });
}

function confirmRotate(hash){
  var u = userByHash(hash); if(!u) return;
  openConfirm("Rotate key",
    'Generate a new key for <strong>'+esc(u.name)+'</strong> (<code>'+esc(u.masked)+'</code>)? The current key stops working immediately — anything still using it gets 401s until updated.',
    activitySummary(u), "Rotate key",
    function(){
      apiPost("/admin/users/mutate", {action:"rotate", key_hash: hash}).then(function(res){
        if(!res.ok){ flash(res.json.error && res.json.error.message || "Rotate failed", "error"); return; }
        showKeyModal(u.name, res.json.key, "rotated");
        load();
      });
    });
}

function confirmDelete(hash){
  var u = userByHash(hash); if(!u) return;
  openConfirm("Delete key",
    'Delete <strong>'+esc(u.name)+'</strong> (<code>'+esc(u.masked)+'</code>)? Access is revoked immediately and the key cannot be recovered.',
    activitySummary(u), "Delete key",
    function(){
      apiPost("/admin/users/mutate", {action:"delete", key_hash: hash}).then(function(res){
        if(!res.ok){ flash(res.json.error && res.json.error.message || "Delete failed", "error"); return; }
        flash("Deleted", "success");
        load();
      });
    });
}

function fetchKey(keyHash){
  return apiPost("/admin/users/mutate", {action:"reveal", key_hash: keyHash}).then(function(res){
    if(!res.ok) throw new Error(res.json.error && res.json.error.message || "Reveal failed");
    return res.json.key;
  });
}

function revealKey(keyHash, name){
  fetchKey(keyHash).then(function(key){
    showKeyModal(name, key, "reveal");
  }).catch(function(e){ flash(e.message, "error"); });
}

function removeModel(keyHash, model){
  var user = state.users.find(function(u){return u.key_hash === keyHash;});
  if(!user) return;
  var next = (user.models || []).filter(function(m){return m !== model;});
  submitModels(keyHash, next);
}

function showAddModel(keyHash, btn){
  closeModelPicker();
  var user = userByHash(keyHash); if(!user) return;
  var have = user.models || [];
  var opts = state.allModels.filter(function(m){ return have.indexOf(m) < 0; });
  var wrap = document.createElement("span");
  wrap.className = "picker"; wrap.id = "modelPicker";
  var input = document.createElement("input");
  input.type = "text"; input.className = "model-input"; input.placeholder = "type to filter\u2026"; input.autocomplete = "off";
  var menu = document.createElement("div"); menu.className = "picker-menu";
  function renderList(q){
    q = (q||"").toLowerCase();
    var items = opts.filter(function(m){ return !q || m.toLowerCase().indexOf(q) >= 0; });
    menu.innerHTML = items.length
      ? items.map(function(m){ return '<div class="picker-item" data-val="'+escAttr(m)+'">'+esc(m)+'</div>'; }).join('')
      : '<div class="picker-empty">No matching models</div>';
  }
  renderList("");
  input.addEventListener("input", function(){ renderList(input.value); });
  menu.addEventListener("mousedown", function(ev){
    var t = ev.target.closest(".picker-item");
    if(!t) return;
    ev.preventDefault();
    pickModel(keyHash, t.getAttribute("data-val"));
  });
  input.addEventListener("keydown", function(ev){
    if(ev.key === "Escape"){ closeModelPicker(); }
    else if(ev.key === "Enter"){
      ev.preventDefault();
      var q = input.value.trim().toLowerCase();
      var exact = opts.filter(function(m){ return m.toLowerCase() === q; })[0];
      var partial = opts.filter(function(m){ return q && m.toLowerCase().indexOf(q) >= 0; })[0];
      var match = exact || partial;
      if(match) pickModel(keyHash, match);
    }
  });
  wrap.appendChild(input); wrap.appendChild(menu);
  btn.style.display = "none";
  btn.parentElement.appendChild(wrap);
  input.focus();
}

function closeModelPicker(){
  var p = document.getElementById("modelPicker");
  if(p){
    var btn = p.parentElement.querySelector(".pill-add");
    if(btn) btn.style.display = "";
    p.remove();
  }
}

function pickModel(keyHash, val){
  closeModelPicker();
  var user = userByHash(keyHash); if(!user) return;
  var next = (user.models || []).slice();
  if(next.indexOf(val) < 0) next.push(val);
  submitModels(keyHash, next);
}

document.addEventListener("mousedown", function(ev){
  var p = document.getElementById("modelPicker");
  if(p && !p.contains(ev.target)) closeModelPicker();
});

function submitModels(keyHash, models){
  apiPost("/admin/users/mutate", {action:"update_models", key_hash: keyHash, models: models}).then(function(res){
    if(!res.ok){ flash(res.json.error && res.json.error.message || "Update failed", "error"); load(); return; }
    flash("Updated", "success");
    load();
  });
}

attachFilter("userFilter", "usersBody");
load();
`
}
