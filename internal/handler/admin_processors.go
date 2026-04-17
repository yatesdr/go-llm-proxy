package handler

import (
	"log/slog"
	"net/http"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

// ProcessorsPage renders the /admin/processors HTML page.
func (h *AdminHandler) ProcessorsPage(w http.ResponseWriter, r *http.Request) {
	body := `<div class="toolbar"><h2>Processors</h2></div>
<div class="card">
  <p class="hint" style="margin-bottom:14px">Global pipeline processors. These handle content types the target chat model can't natively process — e.g. images sent to a text-only model get routed through the vision processor first.</p>
  <form id="procForm" onsubmit="submitProcessors(event)">
    <div class="field-grid">
      <div class="field field-full">
        <label>Vision (image) model <span class="tip" tabindex="0" data-tip="Default: empty (images rejected when target model lacks supports_vision). Select a vision-capable model to auto-describe images for text-only backends.">?</span></label>
        <select name="vision" id="visionSelect"></select>
        <div class="hint" id="visionHint" style="margin-top:4px"></div>
      </div>
      <div class="field field-full">
        <label>Audio (transcription) model <span class="tip" tabindex="0" data-tip="Default: empty (audio not pre-processed). Select a whisper-style transcription model to auto-transcribe input_audio for text-only backends. Audio pipeline integration is pending — this configures the field; the pipeline stage is a follow-up.">?</span></label>
        <select name="audio" id="audioSelect"></select>
        <div class="hint" id="audioHint" style="margin-top:4px">Pipeline integration pending — value is persisted to config but not yet active.</div>
      </div>
      <div class="field field-full">
        <label>Extraction (OCR) model <span class="tip" tabindex="0" data-tip="Default: falls back to the vision model. Used for PDF page rasterization + text extraction. Override only if a dedicated OCR model (e.g. PaddleOCR-VL) outperforms your vision model for document scans.">?</span></label>
        <select name="ocr" id="ocrSelect"></select>
      </div>
      <div class="field field-full">
        <label>Web search key <span class="tip" tabindex="0" data-tip="Tavily or Brave Search API key. Single provider only for now — multi-provider cascading is a planned enhancement. Leave blank to disable web search injection.">?</span></label>
        <div class="secret-row">
          <span class="mono" id="searchKeyMask">—</span>
          <button type="button" class="btn btn-secondary btn-sm" onclick="rotateProcessorsSecret(this)">Rotate</button>
          <button type="button" class="btn btn-danger btn-sm" onclick="clearProcessorsSecret()">Clear</button>
        </div>
        <input type="password" name="web_search_key" id="searchKeyInput" style="display:none;margin-top:6px" placeholder="enter new key (tvly-... or brave-...) — leave blank to keep existing">
      </div>
    </div>
    <div id="procFormErr" class="inline-err" style="display:none;margin-top:10px"></div>
    <div class="btn-row" style="margin-top:18px">
      <button type="submit" class="btn btn-primary">Save processors</button>
    </div>
  </form>
</div>`
	script := processorsPageJS()
	h.renderShell(w, "processors", "Admin · Processors", body, script)
}

// ProcessorsData serves GET /admin/processors/data.
func (h *AdminHandler) ProcessorsData(w http.ResponseWriter, r *http.Request) {
	cfg := h.cs.Get()
	all := make([]string, 0, len(cfg.Models))
	visionCapable := make([]string, 0)
	audioCapable := make([]string, 0)
	for _, m := range cfg.Models {
		all = append(all, m.Name)
		if m.SupportsVision {
			visionCapable = append(visionCapable, m.Name)
		}
		if m.SupportsAudio {
			audioCapable = append(audioCapable, m.Name)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"processors": map[string]any{
			"vision":          cfg.Processors.Vision,
			"audio":           cfg.Processors.Audio,
			"ocr":             cfg.Processors.OCR,
			"has_search_key":  cfg.Processors.WebSearchKey != "",
			"search_key_mask": config.MaskSecret(cfg.Processors.WebSearchKey),
		},
		"all_models":     all,
		"vision_capable": visionCapable,
		"audio_capable":  audioCapable,
	})
}

// ProcessorsMutate handles POST /admin/processors/mutate.
// Body: {"vision":"...","audio":"...","ocr":"...","web_search_key":*string}
// web_search_key is a pointer-style field: omit to keep current, empty string to clear.
func (h *AdminHandler) ProcessorsMutate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Vision       string  `json:"vision"`
		Audio        string  `json:"audio"`
		OCR          string  `json:"ocr"`
		WebSearchKey *string `json:"web_search_key"`
	}
	if err := decodeJSONBody(r, &req, 16*1024); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cur := h.cs.Get().Processors
	p := config.ProcessorsConfig{
		Vision: req.Vision,
		Audio:  req.Audio,
		OCR:    req.OCR,
	}
	if req.WebSearchKey != nil {
		p.WebSearchKey = *req.WebSearchKey
	} else {
		p.WebSearchKey = cur.WebSearchKey
	}
	if err := h.cs.UpdateProcessors(p); err != nil {
		writeMutateError(w, err)
		return
	}
	slog.Info("admin: processors updated",
		"vision", p.Vision, "audio", p.Audio, "ocr", p.OCR,
		"has_search_key", p.WebSearchKey != "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func processorsPageJS() string {
	return `
var procState = {rotateSearchKey: false};

function loadProcessors(){
  apiGet("/admin/processors/data").then(function(d){
    fillProcSelect("visionSelect", d.vision_capable, d.processors.vision, "— disabled —");
    fillProcSelect("audioSelect", d.audio_capable, d.processors.audio, "— disabled —");
    fillProcSelect("ocrSelect", d.all_models, d.processors.ocr, "— same as vision —", true);
    var mask = document.getElementById("searchKeyMask");
    mask.textContent = d.processors.has_search_key ? d.processors.search_key_mask : "none";
    document.getElementById("searchKeyInput").style.display = "none";
    procState.rotateSearchKey = false;
  }).catch(function(e){ flash("Load failed: "+e.message, "error"); });
}

function fillProcSelect(id, options, selected, emptyLabel, allowNone){
  var sel = document.getElementById(id);
  sel.innerHTML = "";
  var opt0 = document.createElement("option");
  opt0.value = ""; opt0.textContent = emptyLabel;
  sel.appendChild(opt0);
  if(allowNone){
    var optN = document.createElement("option");
    optN.value = "none"; optN.textContent = "— disabled —";
    sel.appendChild(optN);
  }
  var seen = {};
  for(var i=0;i<options.length;i++){
    seen[options[i]] = true;
    var o = document.createElement("option");
    o.value = options[i]; o.textContent = options[i];
    sel.appendChild(o);
  }
  // If the currently-selected value isn't in the capability-filtered list,
  // add it anyway so we don't silently drop a saved value.
  if(selected && selected !== "" && selected !== "none" && !seen[selected]){
    var o = document.createElement("option");
    o.value = selected; o.textContent = selected + " (not marked capable)";
    sel.appendChild(o);
  }
  sel.value = selected || "";
}

function rotateProcessorsSecret(btn){
  var input = document.getElementById("searchKeyInput");
  input.style.display = "block";
  input.focus();
  procState.rotateSearchKey = true;
  btn.disabled = true;
}

function clearProcessorsSecret(){
  document.getElementById("searchKeyMask").textContent = "(will be cleared on save)";
  document.getElementById("searchKeyInput").style.display = "none";
  document.getElementById("searchKeyInput").value = "";
  procState.rotateSearchKey = "clear";
}

function submitProcessors(ev){
  ev.preventDefault();
  var form = document.getElementById("procForm");
  var body = {
    vision: form.elements["vision"].value,
    audio: form.elements["audio"].value,
    ocr: form.elements["ocr"].value
  };
  // Secret semantics:
  //   "clear" → send "" to explicitly wipe the key
  //   true    → send trimmed value ONLY if non-empty (blank rotate is a no-op;
  //             user must type a new key or use Clear to wipe — prevents
  //             accidental deletion when Save is clicked with empty input)
  //   false   → omit field; server preserves current value
  var rotateRequested = procState.rotateSearchKey === true;
  var rotateBlank = false;
  if(procState.rotateSearchKey === "clear"){
    body.web_search_key = "";
  } else if(rotateRequested){
    var v = form.elements["web_search_key"].value.trim();
    if(v !== ""){
      body.web_search_key = v;
    } else {
      rotateBlank = true;
    }
  }
  apiPost("/admin/processors/mutate", body).then(function(res){
    if(!res.ok){
      var msg = (res.json && res.json.error && res.json.error.message) || "Save failed";
      var el = document.getElementById("procFormErr");
      el.textContent = msg; el.style.display = "block";
      return;
    }
    document.getElementById("procFormErr").style.display = "none";
    if(rotateBlank){
      flash("Saved — web search key unchanged (rotate input was blank). Use Clear to remove it.", "info");
    } else {
      flash("Processors saved", "success");
    }
    loadProcessors();
  });
}

loadProcessors();
`
}
