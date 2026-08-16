package handler

import (
	"log/slog"
	"net/http"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

// UsersPage renders the /admin/users HTML page.
func (h *AdminHandler) UsersPage(w http.ResponseWriter, r *http.Request) {
	body := `<div class="toolbar">
  <h2>Users</h2>
  <button class="btn btn-primary btn-sm" type="button" onclick="addUserPrompt()">+ Add User</button>
</div>
<div class="card">
  <div class="table-wrap">
    <table class="data-table">
      <thead><tr><th style="width:170px">Name</th><th style="width:170px">Key</th><th>Allowed Models</th><th style="width:210px;text-align:right">Actions</th></tr></thead>
      <tbody id="usersBody"><tr><td colspan="4" style="text-align:center;color:var(--muted)">Loading…</td></tr></tbody>
    </table>
  </div>
</div>
<datalist id="allModels"></datalist>
`
	script := usersPageJS()
	h.renderShell(w, "users", "Admin · Users", body, script)
}

// UsersData serves the JSON payload the /admin/users page fetches on load.
func (h *AdminHandler) UsersData(w http.ResponseWriter, r *http.Request) {
	cfg := h.cs.Get()

	users := make([]map[string]any, 0, len(cfg.Keys))
	for _, k := range cfg.Keys {
		models := k.Models
		if models == nil {
			models = []string{}
		}
		users = append(users, map[string]any{
			"name":     k.Name,
			"key_hash": config.KeyHash(k.Key),
			"masked":   config.MaskKey(k.Key),
			"models":   models,
		})
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
    var dl = document.getElementById("allModels");
    dl.innerHTML = "";
    for(var i=0;i<state.allModels.length;i++){
      var o = document.createElement("option");
      o.value = state.allModels[i];
      dl.appendChild(o);
    }
    renderUsers();
  }).catch(function(e){ flash("Load failed: "+e.message, "error"); });
}

function renderUsers(){
  var tbody = document.getElementById("usersBody");
  if(!state.users.length){
    tbody.innerHTML = '<tr><td colspan="4" style="text-align:center;color:var(--muted)">No users configured</td></tr>';
    return;
  }
  var html = "";
  for(var i=0;i<state.users.length;i++){
    html += renderUserRow(state.users[i]);
  }
  tbody.innerHTML = html;
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
    '<td class="cell-name" title="'+esc(u.name)+'"><button class="btn-link" onclick="renameUser(\''+u.key_hash+'\', \''+escAttr(u.name)+'\')" title="Rename">'+esc(u.name)+'</button></td>' +
    '<td class="cell-key"><code>'+esc(u.masked)+'</code></td>' +
    '<td class="cell-pills"><div class="pills-wrap">'+pills+'</div></td>' +
    '<td class="row-actions"><div class="action-group">'+
      '<button class="btn btn-secondary btn-sm" onclick="revealKey(\''+u.key_hash+'\', \''+escAttr(u.name)+'\')" title="Show the full key in a banner">Reveal</button>'+
      '<button class="btn btn-secondary btn-sm" onclick="copyKey(\''+u.key_hash+'\')" title="Copy the full key to the clipboard">Copy</button>'+
      '<button class="btn btn-secondary btn-sm" onclick="rotateUser(\''+u.key_hash+'\', \''+escAttr(u.name)+'\')" title="Replace this key with a new one; the old key stops working immediately">Rotate</button>'+
      '<button class="btn btn-danger btn-sm" onclick="deleteUser(\''+u.key_hash+'\', \''+escAttr(u.name)+'\')" title="Delete this key">&times;</button>'+
    '</div></td></tr>';
}

function fetchKey(keyHash){
  return apiPost("/admin/users/mutate", {action:"reveal", key_hash: keyHash}).then(function(res){
    if(!res.ok) throw new Error(res.json.error && res.json.error.message || "Reveal failed");
    return res.json.key;
  });
}

function revealKey(keyHash, name){
  fetchKey(keyHash).then(function(key){
    showPersistentBanner(
      '<strong>API key for <code>'+esc(name)+'</code>:</strong><br>' +
      '<code id="revealKeyVal">'+esc(key)+'</code>' +
      '<div class="persist-actions">' +
      '<button class="btn btn-primary btn-sm" type="button" onclick="copyToClipboard(document.getElementById(\'revealKeyVal\').textContent).then(function(){flash(\'Copied to clipboard\',\'success\');})">Copy</button>' +
      '<button class="btn btn-secondary btn-sm" type="button" onclick="dismissPersistent()">Dismiss</button>' +
      '</div>'
    );
  }).catch(function(e){ flash(e.message, "error"); });
}

function copyKey(keyHash){
  fetchKey(keyHash).then(function(key){
    return copyToClipboard(key);
  }).then(function(){
    flash("Key copied to clipboard", "success");
  }).catch(function(e){ flash(e.message || "Copy failed", "error"); });
}

function rotateUser(keyHash, name){
  if(!confirm('Rotate the key for "'+name+'"?\n\nA new key will be generated and shown once. The current key stops working immediately — anything still using it will get 401s until updated.')) return;
  apiPost("/admin/users/mutate", {action:"rotate", key_hash: keyHash}).then(function(res){
    if(!res.ok){ flash(res.json.error && res.json.error.message || "Rotate failed", "error"); return; }
    var j = res.json;
    showPersistentBanner(
      '<strong>New API key for <code>'+esc(name)+'</code> (the old key is now invalid):</strong><br>' +
      '<code id="rotatedKeyVal">'+esc(j.key)+'</code>' +
      '<div class="persist-actions">' +
      '<button class="btn btn-primary btn-sm" type="button" onclick="copyToClipboard(document.getElementById(\'rotatedKeyVal\').textContent).then(function(){flash(\'Copied to clipboard\',\'success\');})">Copy</button>' +
      '<button class="btn btn-secondary btn-sm" type="button" onclick="dismissPersistent()">Dismiss</button>' +
      '</div>'
    );
    load();
  });
}

function addUserPrompt(){
  var name = prompt("Name for the new user / key:");
  if(!name) return;
  name = name.trim();
  if(!name) return;
  apiPost("/admin/users/mutate", {action:"add", name: name}).then(function(res){
    if(!res.ok){ flash(res.json.error && res.json.error.message || "Add failed", "error"); return; }
    var j = res.json;
    showPersistentBanner(
      '<strong>New API key for <code>'+esc(j.name)+'</code>:</strong><br>' +
      '<code id="newKeyVal">'+esc(j.key)+'</code>' +
      '<div class="persist-actions">' +
      '<button class="btn btn-primary btn-sm" type="button" onclick="copyToClipboard(document.getElementById(\'newKeyVal\').textContent).then(function(){flash(\'Copied to clipboard\',\'success\');})">Copy</button>' +
      '<button class="btn btn-secondary btn-sm" type="button" onclick="dismissPersistent()">Dismiss</button>' +
      '<span style="color:var(--muted);font-size:.82rem;align-self:center">Recoverable later via the reveal button.</span>' +
      '</div>'
    );
    load();
  });
}

function removeModel(keyHash, model){
  var user = state.users.find(function(u){return u.key_hash === keyHash;});
  if(!user) return;
  var next = (user.models || []).filter(function(m){return m !== model;});
  submitModels(keyHash, next);
}

function showAddModel(keyHash, btn){
  var row = btn.parentElement;
  var input = document.createElement("input");
  input.type = "text";
  input.className = "model-input";
  input.setAttribute("list", "allModels");
  input.placeholder = "type to add…";
  input.autofocus = true;
  btn.style.display = "none";
  row.appendChild(input);
  input.focus();
  var done = false;
  function commit(){
    if(done) return; done = true;
    var val = input.value.trim();
    input.remove(); btn.style.display = "";
    if(!val) return;
    if(!state.allModels.includes(val)){
      flash('Unknown model "'+val+'"', "error"); return;
    }
    var user = state.users.find(function(u){return u.key_hash === keyHash;});
    if(!user) return;
    var next = (user.models || []).slice();
    if(!next.includes(val)) next.push(val);
    submitModels(keyHash, next);
  }
  input.addEventListener("keydown", function(ev){
    if(ev.key === "Enter"){ ev.preventDefault(); commit(); }
    else if(ev.key === "Escape"){ done = true; input.remove(); btn.style.display=""; }
  });
  input.addEventListener("blur", commit);
}

function submitModels(keyHash, models){
  apiPost("/admin/users/mutate", {action:"update_models", key_hash: keyHash, models: models}).then(function(res){
    if(!res.ok){ flash(res.json.error && res.json.error.message || "Update failed", "error"); load(); return; }
    flash("Updated", "success");
    load();
  });
}

function renameUser(keyHash, current){
  var name = prompt("New name:", current);
  if(!name || name === current) return;
  apiPost("/admin/users/mutate", {action:"rename", key_hash: keyHash, name: name.trim()}).then(function(res){
    if(!res.ok){ flash(res.json.error && res.json.error.message || "Rename failed", "error"); return; }
    load();
  });
}

function deleteUser(keyHash, name){
  if(!confirm('Delete key "'+name+'"? This immediately revokes access.')) return;
  apiPost("/admin/users/mutate", {action:"delete", key_hash: keyHash}).then(function(res){
    if(!res.ok){ flash(res.json.error && res.json.error.message || "Delete failed", "error"); return; }
    flash("Deleted", "success");
    load();
  });
}

load();
`
}
