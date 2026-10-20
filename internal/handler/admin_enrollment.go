package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/mail"
	"time"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/mailer"
)

// sender returns the mailer used for the test email.
func (h *AdminHandler) sender() mailer.Sender {
	if h.Mailer != nil {
		return h.Mailer
	}
	return &mailer.SMTP{}
}

// EnrollmentPage renders /admin/enrollment.
func (h *AdminHandler) EnrollmentPage(w http.ResponseWriter, r *http.Request) {
	body := `<div class="card">
  <div class="card-header"><h2>Self-service keys</h2><div class="card-tools"><span id="enrollStatus" class="st-word"></span></div></div>
  <p class="hint" style="margin-bottom:14px">People open a join link, enter a work email in one of its domains, confirm by email and get their own API key (or replace the one they have). Keys appear on the API Keys tab, named by email.</p>
  <form id="enrollForm" onsubmit="saveSettings(event)">
    <div class="field-grid">
      <div class="field field-full">
        <label for="publicUrl">Public address <span class="tip" tabindex="0" data-tip="The address people reach this proxy at, e.g. https://llm.example.com. Emailed links point here; the request's Host header is never used for them.">?</span></label>
        <input type="url" id="publicUrl" placeholder="https://llm.example.com" autocomplete="off">
        <div class="hint" style="margin-top:4px"><button type="button" class="btn-link" onclick="useThisAddress()">Use this page's address</button></div>
      </div>
      <div class="field field-full checkbox-row" style="padding-top:0"><input type="checkbox" id="enrollPaused"><label for="enrollPaused">Pause every join link</label></div>
    </div>
    <div class="section" style="margin-top:18px"><h3>Email (SMTP)</h3>
    <div class="field-grid">
      <div class="field"><label for="smtpHost">Host</label><input type="text" id="smtpHost" placeholder="smtp.example.com" autocomplete="off"></div>
      <div class="field"><label for="smtpPort">Port</label><input type="number" id="smtpPort" min="1" max="65535" placeholder="587"></div>
      <div class="field"><label for="smtpUser">Username</label><input type="text" id="smtpUser" autocomplete="off"></div>
      <div class="field"><label for="smtpPass">Password <span class="tip" tabindex="0" data-tip="Leave blank to keep the stored password. A password file is preferred: the secret stays out of the config.">?</span></label><input type="password" id="smtpPass" autocomplete="new-password" placeholder="leave blank to keep"><div class="hint" id="smtpPassState" style="margin-top:4px"></div></div>
      <div class="field field-full"><label for="smtpPassFile">Password file <span class="tip" tabindex="0" data-tip="Absolute path to a file holding the password, read at send time. Used instead of the password above.">?</span></label><input type="text" id="smtpPassFile" placeholder="/run/secrets/smtp_password" autocomplete="off"></div>
      <div class="field"><label for="smtpFrom">From</label><input type="text" id="smtpFrom" placeholder="LLM Proxy &lt;no-reply@example.com&gt;" autocomplete="off"></div>
      <div class="field"><label for="smtpReplyTo">Reply-To</label><input type="text" id="smtpReplyTo" placeholder="support@example.com" autocomplete="off"></div>
      <div class="field field-full checkbox-row" style="padding-top:0"><input type="checkbox" id="smtpStartTLS"><label for="smtpStartTLS">Require STARTTLS (recommended)</label></div>
    </div></div>
    <div class="section"><h3>code-link downloads</h3>
    <div class="field-grid">
      <div class="field"><label for="dlMac">macOS</label><input type="url" id="dlMac" autocomplete="off"></div>
      <div class="field"><label for="dlWin">Windows</label><input type="url" id="dlWin" autocomplete="off"></div>
      <div class="field field-full"><label for="dlQuick">Quick start</label><input type="url" id="dlQuick" autocomplete="off"></div>
    </div>
    <p class="hint" style="margin-top:6px">Shown with each new key. Blank uses the default shown.</p></div>
    <div id="enrollErr" class="inline-err" style="display:none"></div>
    <div class="btn-row"><button type="submit" class="btn btn-primary">Save settings</button></div>
  </form>
  <div class="section" style="margin-top:20px;margin-bottom:0"><h3>Send a test email</h3>
    <div class="cascade-add"><input type="email" id="testTo" class="filter-input" placeholder="you@example.com" style="width:260px"><button type="button" class="btn btn-secondary" onclick="sendTest()">Send test email</button></div>
    <p class="hint" style="margin-top:6px">Uses the saved settings.</p>
  </div>
</div>
<div class="card" style="margin-top:16px">
  <div class="card-header"><h2>Join links</h2><div class="card-tools"><button class="btn btn-primary" type="button" onclick="openJoinModal()">+ New join link</button></div></div>
  <div class="table-wrap">
    <table class="data-table">
      <thead><tr><th style="width:170px">Name</th><th>Email domains</th><th>Models</th><th style="width:70px;text-align:right">Keys</th><th style="width:80px">Status</th><th style="width:250px;text-align:right">Actions</th></tr></thead>
      <tbody id="joinBody"><tr><td colspan="6" class="empty-cell">Loading…</td></tr></tbody>
    </table>
  </div>
</div>
<div id="joinModal" class="modal-backdrop" onclick="if(event.target.id==='joinModal')closeJoinModal()">
  <div class="modal" style="max-width:480px" role="dialog">
    <div class="modal-header"><h2 id="joinModalTitle">New join link</h2><button class="modal-close" type="button" onclick="closeJoinModal()">&times;</button></div>
    <form onsubmit="submitJoinModal(event)">
      <div class="modal-body">
        <div class="field"><label for="jkName">Name</label><input type="text" id="jkName" required maxlength="64" autocomplete="off" placeholder="e.g. the company"></div>
        <div class="field"><label for="jkDomains">Email domains</label><input type="text" id="jkDomains" required autocomplete="off" placeholder="example.com, example.org"><div class="hint" style="margin-top:4px">Exact domains, separated by commas. Only these addresses can get a key.</div></div>
        <div class="field"><label>Models</label><div id="jkModels" class="pills-wrap"></div><div class="hint" style="margin-top:4px">None ticked = every model.</div></div>
        <div class="field checkbox-row" style="padding-top:0"><input type="checkbox" id="jkPublic"><label for="jkPublic">Public: also accept these domains at <span class="mono" id="jkPublicUrl">/join</span></label></div>
        <p class="hint">code-link's "Get a key" link opens that page. A domain can belong to only one public join link.</p>
        <div id="joinModalErr" class="inline-err" style="display:none"></div>
      </div>
      <div class="modal-footer"><button type="button" class="btn btn-secondary" onclick="closeJoinModal()">Cancel</button><button class="btn btn-primary" type="submit" id="joinModalSave">Create</button></div>
    </form>
  </div>
</div>
`
	h.renderShell(w, "enrollment", "Admin · Enrollment", body, enrollmentPageJS())
}

// EnrollmentData serves GET /admin/enrollment/data. The SMTP password is
// never sent; only whether one is set.
func (h *AdminHandler) EnrollmentData(w http.ResponseWriter, r *http.Request) {
	cfg := h.cs.Get()
	e := cfg.Enrollment
	issued := make(map[string]int)
	for _, k := range cfg.Keys {
		if k.JoinKey != "" {
			issued[k.JoinKey]++
		}
	}
	joinKeys := make([]map[string]any, 0, len(e.JoinKeys))
	for _, j := range e.JoinKeys {
		models := j.Models
		if models == nil {
			models = []string{}
		}
		joinKeys = append(joinKeys, map[string]any{
			"id": j.ID, "name": j.Name, "key": j.Key, "domains": j.Domains,
			"models": models, "paused": j.Paused, "public": j.Public, "keys": issued[j.ID],
		})
	}
	passwordSource := ""
	switch {
	case e.SMTP.PasswordFile != "":
		passwordSource = "file"
	case e.SMTP.Password != "":
		passwordSource = "inline"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": map[string]any{
			"public_url": e.PublicURL,
			"paused":     e.Paused,
			"smtp": map[string]any{
				"host": e.SMTP.Host, "port": e.SMTP.EffectivePort(), "username": e.SMTP.Username,
				"password_set": e.SMTP.PasswordSet(), "password_source": passwordSource,
				"password_file": e.SMTP.PasswordFile, "from": e.SMTP.From, "reply_to": e.SMTP.ReplyTo,
				"starttls": e.SMTP.StartTLS(),
			},
			"downloads": map[string]any{
				"macos": e.Downloads.MacOS, "windows": e.Downloads.Windows, "quickstart": e.Downloads.Quickstart,
			},
		},
		"defaults": map[string]any{
			"macos": config.DefaultDownloadMacOS, "windows": config.DefaultDownloadWindows,
			"quickstart": config.DefaultDownloadQuickstart,
		},
		"missing":    append([]string{}, e.Missing()...),
		"join_keys":  joinKeys,
		"all_models": keyModelNames(cfg),
	})
}

// keyModelNames lists every model name an API key may be limited to.
func keyModelNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Models)+2)
	for _, m := range cfg.Models {
		out = append(out, m.Name)
	}
	if cfg.Audio.Whisper != nil {
		out = append(out, cfg.Audio.Whisper.Name)
	}
	if cfg.Audio.TTS != nil {
		out = append(out, cfg.Audio.TTS.Name)
	}
	return out
}

type enrollmentSettingsDTO struct {
	PublicURL string `json:"public_url"`
	Paused    bool   `json:"paused"`
	SMTP      struct {
		Host         string `json:"host"`
		Port         int    `json:"port"`
		Username     string `json:"username"`
		Password     string `json:"password"` // blank keeps the stored one
		PasswordFile string `json:"password_file"`
		From         string `json:"from"`
		ReplyTo      string `json:"reply_to"`
		STARTTLS     bool   `json:"starttls"`
	} `json:"smtp"`
	Downloads struct {
		MacOS      string `json:"macos"`
		Windows    string `json:"windows"`
		Quickstart string `json:"quickstart"`
	} `json:"downloads"`
}

// EnrollmentMutate handles POST /admin/enrollment/mutate.
func (h *AdminHandler) EnrollmentMutate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action   string                 `json:"action"`
		ID       string                 `json:"id"`
		Name     string                 `json:"name"`
		Domains  []string               `json:"domains"`
		Models   []string               `json:"models"`
		Paused   bool                   `json:"paused"`
		Public   bool                   `json:"public"`
		To       string                 `json:"to"`
		Settings *enrollmentSettingsDTO `json:"settings"`
	}
	if err := decodeJSONBody(r, &req, 32*1024); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	switch req.Action {
	case "save_settings":
		if req.Settings == nil {
			httputil.WriteError(w, http.StatusBadRequest, "settings are required")
			return
		}
		s := req.Settings
		starttls := s.SMTP.STARTTLS
		err := h.cs.UpdateEnrollmentSettings(config.EnrollmentSettings{
			PublicURL: s.PublicURL,
			Paused:    s.Paused,
			SMTP: config.SMTPConfig{
				Host: s.SMTP.Host, Port: s.SMTP.Port, Username: s.SMTP.Username, Password: s.SMTP.Password,
				PasswordFile: s.SMTP.PasswordFile, From: s.SMTP.From, ReplyTo: s.SMTP.ReplyTo, STARTTLS: &starttls,
			},
			Downloads: config.DownloadsConfig{MacOS: s.Downloads.MacOS, Windows: s.Downloads.Windows, Quickstart: s.Downloads.Quickstart},
		})
		if err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: enrollment settings updated", "paused", s.Paused)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "test_email":
		to, err := mail.ParseAddress(req.To)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid email address")
			return
		}
		cfg := h.cs.Get()
		settings, err := MailSettings(cfg)
		if err != nil {
			httputil.WriteError(w, http.StatusBadGateway, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		err = h.sender().Send(ctx, settings, mailer.Message{
			To:      to.Address,
			Subject: "Test email from " + publicHost(cfg),
			Text:    "This is a test email from the LLM proxy's enrollment settings. Join-link confirmations will be sent the same way.\n",
		})
		if err != nil {
			slog.Warn("admin: test email failed", "error", err)
			httputil.WriteError(w, http.StatusBadGateway, err.Error())
			return
		}
		slog.Info("admin: test email sent")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "add_join_key":
		j, err := h.cs.AddJoinKey(req.Name, req.Domains, req.Models, req.Public)
		if err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: join key added", "id", j.ID, "domains", j.Domains)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": j.ID, "key": j.Key})

	case "update_join_key":
		if err := h.cs.UpdateJoinKey(req.ID, req.Name, req.Domains, req.Models, req.Public); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: join key updated", "id", req.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "pause_join_key":
		if err := h.cs.SetJoinKeyPaused(req.ID, req.Paused); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: join key paused", "id", req.ID, "paused", req.Paused)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "delete_join_key":
		if err := h.cs.DeleteJoinKey(req.ID); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: join key deleted", "id", req.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		httputil.WriteError(w, http.StatusBadRequest, "unknown action")
	}
}

func enrollmentPageJS() string {
	return `
var enr = {data: null, editId: null};

function loadEnrollment(){
  apiGet("/admin/enrollment/data").then(function(d){
    enr.data = d;
    fillSettings(d);
    renderJoinKeys();
  }).catch(function(e){ flash("Load failed: "+e.message, "error"); });
}

function fillSettings(d){
  var s = d.settings, m = s.smtp;
  document.getElementById("publicUrl").value = s.public_url || "";
  document.getElementById("enrollPaused").checked = !!s.paused;
  document.getElementById("smtpHost").value = m.host || "";
  document.getElementById("smtpPort").value = m.port || "";
  document.getElementById("smtpUser").value = m.username || "";
  document.getElementById("smtpPass").value = "";
  document.getElementById("smtpPassFile").value = m.password_file || "";
  document.getElementById("smtpFrom").value = m.from || "";
  document.getElementById("smtpReplyTo").value = m.reply_to || "";
  document.getElementById("smtpStartTLS").checked = !!m.starttls;
  var ps = document.getElementById("smtpPassState");
  ps.textContent = m.password_source === "file" ? "Password read from the password file." :
    (m.password_source === "inline" ? "A password is set." : "No password set.");
  document.getElementById("dlMac").value = s.downloads.macos || "";
  document.getElementById("dlWin").value = s.downloads.windows || "";
  document.getElementById("dlQuick").value = s.downloads.quickstart || "";
  document.getElementById("dlMac").placeholder = d.defaults.macos;
  document.getElementById("dlWin").placeholder = d.defaults.windows;
  document.getElementById("dlQuick").placeholder = d.defaults.quickstart;
  var st = document.getElementById("enrollStatus");
  if(s.paused){ st.innerHTML = '<span class="health-dot health-degraded"></span>Paused'; }
  else if(d.missing.length){ st.innerHTML = '<span class="health-dot health-offline"></span>Not ready: needs '+esc(d.missing.join(", ")); }
  else { st.innerHTML = '<span class="health-dot health-online"></span>Ready'; }
}

function useThisAddress(){ document.getElementById("publicUrl").value = window.location.origin; }

function saveSettings(ev){
  ev.preventDefault();
  var body = {action: "save_settings", settings: {
    public_url: document.getElementById("publicUrl").value.trim(),
    paused: document.getElementById("enrollPaused").checked,
    smtp: {
      host: document.getElementById("smtpHost").value.trim(),
      port: parseInt(document.getElementById("smtpPort").value, 10) || 0,
      username: document.getElementById("smtpUser").value.trim(),
      password: document.getElementById("smtpPass").value,
      password_file: document.getElementById("smtpPassFile").value.trim(),
      from: document.getElementById("smtpFrom").value.trim(),
      reply_to: document.getElementById("smtpReplyTo").value.trim(),
      starttls: document.getElementById("smtpStartTLS").checked
    },
    downloads: {
      macos: document.getElementById("dlMac").value.trim(),
      windows: document.getElementById("dlWin").value.trim(),
      quickstart: document.getElementById("dlQuick").value.trim()
    }
  }};
  var err = document.getElementById("enrollErr");
  apiPost("/admin/enrollment/mutate", body).then(function(res){
    if(!res.ok){ err.textContent = res.json.error && res.json.error.message || "Save failed"; err.style.display = "block"; return; }
    err.style.display = "none";
    flash("Saved", "success");
    loadEnrollment();
  });
}

function sendTest(){
  var to = document.getElementById("testTo").value.trim();
  if(!to){ flash("Enter an address to send to", "error"); return; }
  flash("Sending…", "info");
  apiPost("/admin/enrollment/mutate", {action: "test_email", to: to}).then(function(res){
    if(!res.ok){ flash(res.json.error && res.json.error.message || "Send failed", "error"); return; }
    flash("Test email sent to "+to, "success");
  });
}

function joinLink(j){
  var base = (enr.data.settings.public_url || window.location.origin).replace(/\/+$/, "");
  return base + "/join/" + j.key;
}

function joinById(id){ return (enr.data.join_keys||[]).find(function(j){ return j.id === id; }); }

function renderJoinKeys(){
  var tbody = document.getElementById("joinBody");
  var list = enr.data.join_keys || [];
  if(!list.length){
    tbody.innerHTML = '<tr><td colspan="6" class="empty-cell">No join links yet. Create one to let people get their own key.</td></tr>';
    return;
  }
  var html = "";
  for(var i=0;i<list.length;i++){
    var j = list[i];
    var domains = j.domains.map(function(d){ return '<span class="pill" style="padding-right:7px">@'+esc(d)+'</span>'; }).join("");
    var models = j.models.length ? j.models.map(function(m){ return '<span class="pill" style="padding-right:7px">'+esc(m)+'</span>'; }).join("")
      : '<span class="pill pill-unrestricted">all models</span>';
    var status = j.paused ? '<span class="health-dot health-degraded"></span><span class="st-word">Paused</span>'
      : '<span class="health-dot health-online"></span><span class="st-word">Active</span>';
    if(j.public) status += '<div class="hint" title="Also accepted on the bare /join page">Public</div>';
    html += '<tr>'+
      '<td class="cell-name">'+esc(j.name)+'<div class="hint">'+esc(j.id)+'</div></td>'+
      '<td class="cell-pills"><div class="pills-wrap">'+domains+'</div></td>'+
      '<td class="cell-pills"><div class="pills-wrap">'+models+'</div></td>'+
      '<td class="mono" style="text-align:right">'+Number(j.keys||0)+'</td>'+
      '<td>'+status+'</td>'+
      '<td class="row-actions"><div class="action-group">'+
        '<button class="btn btn-secondary btn-sm" onclick="copyJoinLink(\''+escAttr(j.id)+'\')" title="Copy the join link">Copy link</button>'+
        '<button class="btn btn-secondary btn-sm" onclick="openJoinModal(\''+escAttr(j.id)+'\')">Edit</button>'+
        '<button class="btn btn-secondary btn-sm" onclick="togglePause(\''+escAttr(j.id)+'\')">'+(j.paused ? "Resume" : "Pause")+'</button>'+
        '<button class="btn btn-danger btn-sm btn-icon" onclick="confirmDeleteJoin(\''+escAttr(j.id)+'\')" title="Delete join link"><svg viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><path d="M2.5 4h11M5.5 4V2.5h5V4M4 4l.7 9.5h6.6L12 4M6.5 6.8v4.4M9.5 6.8v4.4"/></svg></button>'+
      '</div></td></tr>';
  }
  tbody.innerHTML = html;
}

function copyJoinLink(id){
  var j = joinById(id); if(!j) return;
  copyToClipboard(joinLink(j)).then(function(){ flash("Join link copied", "success"); });
}

function togglePause(id){
  var j = joinById(id); if(!j) return;
  apiPost("/admin/enrollment/mutate", {action: "pause_join_key", id: id, paused: !j.paused}).then(function(res){
    if(!res.ok){ flash(res.json.error && res.json.error.message || "Update failed", "error"); return; }
    flash(j.paused ? "Resumed" : "Paused", "success");
    loadEnrollment();
  });
}

function confirmDeleteJoin(id){
  var j = joinById(id); if(!j) return;
  openConfirm("Delete join link",
    'Delete the join link <strong>'+esc(j.name)+'</strong>? The link stops working. The '+Number(j.keys||0)+' key(s) it issued keep working; revoke them on the API Keys tab.',
    "", "Delete link",
    function(){
      apiPost("/admin/enrollment/mutate", {action: "delete_join_key", id: id}).then(function(res){
        if(!res.ok){ flash(res.json.error && res.json.error.message || "Delete failed", "error"); return; }
        flash("Deleted", "success");
        loadEnrollment();
      });
    });
}

function openJoinModal(id){
  var j = id ? joinById(id) : null;
  enr.editId = j ? j.id : null;
  document.getElementById("joinModalTitle").textContent = j ? "Edit join link" : "New join link";
  document.getElementById("joinModalSave").textContent = j ? "Save" : "Create";
  document.getElementById("jkName").value = j ? j.name : "";
  document.getElementById("jkDomains").value = j ? j.domains.join(", ") : "";
  document.getElementById("jkPublic").checked = j ? !!j.public : false;
  document.getElementById("jkPublicUrl").textContent = (enr.data.settings.public_url || window.location.origin).replace(/\/+$/, "") + "/join";
  var have = j ? j.models : [];
  var box = document.getElementById("jkModels");
  box.innerHTML = (enr.data.all_models||[]).map(function(m){
    return '<label class="pill" style="padding-right:7px;cursor:pointer"><input type="checkbox" value="'+esc(m)+'"'+(have.indexOf(m)>=0?" checked":"")+'> '+esc(m)+'</label>';
  }).join("") || '<span class="hint">No models configured.</span>';
  document.getElementById("joinModalErr").style.display = "none";
  document.getElementById("joinModal").classList.add("open");
  document.getElementById("jkName").focus();
}
function closeJoinModal(){ document.getElementById("joinModal").classList.remove("open"); }

function submitJoinModal(ev){
  ev.preventDefault();
  var domains = document.getElementById("jkDomains").value.split(/[\s,;]+/).filter(function(d){ return d; });
  var models = Array.prototype.map.call(document.querySelectorAll("#jkModels input:checked"), function(c){ return c.value; });
  var body = {action: enr.editId ? "update_join_key" : "add_join_key", id: enr.editId || "",
    name: document.getElementById("jkName").value.trim(), domains: domains, models: models,
    public: document.getElementById("jkPublic").checked};
  apiPost("/admin/enrollment/mutate", body).then(function(res){
    if(!res.ok){
      var err = document.getElementById("joinModalErr");
      err.textContent = res.json.error && res.json.error.message || "Save failed"; err.style.display = "block";
      return;
    }
    closeJoinModal();
    flash(enr.editId ? "Saved" : "Join link created — use Copy link to share it", "success");
    loadEnrollment();
  });
}

document.addEventListener("keydown", function(e){ if(e.key === "Escape") closeJoinModal(); });
loadEnrollment();
`
}
