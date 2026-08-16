package handler

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

func (h *AdminHandler) AudioPage(w http.ResponseWriter, r *http.Request) {
	body := `<div class="toolbar"><div><h2>Audio</h2><p class="helper-text">Whisper serves transcription and translation. TTS serves speech generation. Each service owns its backend servers.</p></div></div>
<div id="audioEditors"><div class="card empty-cell">Loading…</div></div>`
	h.renderShell(w, "audio", "Admin · Audio", body, audioPageJS())
}

func (h *AdminHandler) AudioData(w http.ResponseWriter, r *http.Request) {
	cfg := h.cs.Get()
	var health map[string]config.BackendHealth
	if h.health != nil {
		health = h.health.GetBackendStatus()
	}
	encode := func(a *config.AudioModelConfig) any {
		if a == nil {
			return nil
		}
		return map[string]any{"name": a.Name, "model": a.Model, "timeout": a.Timeout, "backends": backendData(a.Backends, health)}
	}
	writeJSON(w, http.StatusOK, map[string]any{"whisper": encode(cfg.Audio.Whisper), "tts": encode(cfg.Audio.TTS)})
}

func (h *AdminHandler) AudioMutate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind     string            `json:"kind"`
		Enabled  bool              `json:"enabled"`
		Name     string            `json:"name"`
		Model    string            `json:"model"`
		Timeout  int               `json:"timeout"`
		Backends []backendInputDTO `json:"backends"`
	}
	if err := decodeJSONBody(r, &req, 128*1024); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Kind != "whisper" && req.Kind != "tts" {
		httputil.WriteError(w, http.StatusBadRequest, "kind must be whisper or tts")
		return
	}
	if !req.Enabled {
		if err := h.cs.UpdateAudioModel(req.Kind, nil); err != nil {
			writeMutateError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var existing *config.AudioModelConfig
	if req.Kind == "whisper" {
		existing = h.cs.Get().Audio.Whisper
	} else {
		existing = h.cs.Get().Audio.TTS
	}
	var old []config.BackendConfig
	if existing != nil {
		old = existing.Backends
	}
	a := &config.AudioModelConfig{Name: req.Name, Model: req.Model, Timeout: req.Timeout, Backends: resolveBackendRows(old, req.Backends)}
	if err := h.cs.UpdateAudioModel(req.Kind, a); err != nil {
		writeMutateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *AdminHandler) DocumentsPage(w http.ResponseWriter, r *http.Request) {
	body := `<div class="toolbar"><div><h2>Documents</h2><p class="helper-text">PaddleOCR performs layout-aware extraction through its official <code>/layout-parsing</code> contract.</p></div></div>
<div id="documentEditor"><div class="card empty-cell">Loading…</div></div>`
	h.renderShell(w, "documents", "Admin · Documents", body, documentsPageJS())
}

func (h *AdminHandler) DocumentsData(w http.ResponseWriter, r *http.Request) {
	doc := h.cs.Get().Documents.PaddleOCR
	if doc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"paddleocr": nil})
		return
	}
	var health map[string]config.BackendHealth
	if h.health != nil {
		health = h.health.GetBackendStatus()
	}
	writeJSON(w, http.StatusOK, map[string]any{"paddleocr": map[string]any{
		"endpoint": doc.Endpoint, "health_endpoint": doc.HealthEndpoint,
		"timeout": doc.Timeout, "backends": backendData(doc.Backends, health),
	}})
}

func (h *AdminHandler) DocumentsMutate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled        bool              `json:"enabled"`
		Endpoint       string            `json:"endpoint"`
		HealthEndpoint string            `json:"health_endpoint"`
		Timeout        int               `json:"timeout"`
		Backends       []backendInputDTO `json:"backends"`
	}
	if err := decodeJSONBody(r, &req, 128*1024); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !req.Enabled {
		if err := h.cs.UpdatePaddleOCR(nil); err != nil {
			writeMutateError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var old []config.BackendConfig
	if existing := h.cs.Get().Documents.PaddleOCR; existing != nil {
		old = existing.Backends
	}
	p := &config.PaddleOCRConfig{Endpoint: req.Endpoint, HealthEndpoint: req.HealthEndpoint, Timeout: req.Timeout, Backends: resolveBackendRows(old, req.Backends)}
	if err := h.cs.UpdatePaddleOCR(p); err != nil {
		writeMutateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// WorkloadProbe tests a processor's explicit health path. Unlike the chat
// probe it does not assume /models or any OpenAI response shape.
func (h *AdminHandler) WorkloadProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL, Path, Section string
		APIKey             *string `json:"api_key"`
	}
	if err := decodeJSONBody(r, &req, 16*1024); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	parsed, err := url.Parse(req.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || !strings.HasPrefix(req.Path, "/") {
		httputil.WriteError(w, http.StatusBadRequest, "valid url and absolute health path are required")
		return
	}
	key := ""
	if req.APIKey != nil {
		key = *req.APIKey
	} else {
		key = h.storedWorkloadKey(req.Section, req.URL)
	}
	client := httputil.NewHTTPClient()
	client.Timeout = 10 * time.Second
	baseURL := strings.TrimRight(req.URL, "/")
	if (req.Section == "whisper" || req.Section == "tts") && strings.HasSuffix(baseURL, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	upstream, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, baseURL+req.Path, nil)
	if key != "" {
		upstream.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(upstream)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "status": resp.StatusCode, "detail": strings.TrimSpace(string(body))})
}

func (h *AdminHandler) storedWorkloadKey(section, target string) string {
	cfg := h.cs.Get()
	var backends []config.BackendConfig
	switch section {
	case "whisper":
		if cfg.Audio.Whisper != nil {
			backends = cfg.Audio.Whisper.Backends
		}
	case "tts":
		if cfg.Audio.TTS != nil {
			backends = cfg.Audio.TTS.Backends
		}
	case "paddleocr":
		if cfg.Documents.PaddleOCR != nil {
			backends = cfg.Documents.PaddleOCR.Backends
		}
	}
	for _, b := range backends {
		if b.URL == target {
			return b.APIKey
		}
	}
	return ""
}

func audioPageJS() string {
	return `var audioState={};
function loadAudio(){apiGet('/admin/audio/data').then(function(d){audioState=d;renderAudio();});}
function renderAudio(){document.getElementById('audioEditors').innerHTML=audioCard('whisper','Whisper','Transcription and translation','/v1/audio/transcriptions',audioState.whisper)+audioCard('tts','Text to speech','Speech generation','/v1/audio/speech',audioState.tts);}
function audioCard(kind,title,desc,route,data){data=data||{};return '<form class="card workload-card" data-kind="'+kind+'" onsubmit="saveAudio(event)"><div class="toolbar"><div><h3 style="margin:0">'+title+'</h3><p class="helper-text">'+desc+' · Public route <code>'+route+'</code></p></div><label class="checkbox-row"><input class="wl-enabled" type="checkbox" '+(audioState[kind]?'checked':'')+'> Enabled</label></div><div class="field-grid"><div class="field"><label>Client-facing model name</label><input class="wl-name" value="'+escAttr(data.name||'')+'" placeholder="'+(kind==='whisper'?'whisper-large-v3-turbo':'tts-1')+'"></div><div class="field"><label>Upstream model ID</label><input class="wl-model" value="'+escAttr(data.model||'')+'" placeholder="same as client-facing name"></div><div class="field"><label>Timeout (seconds)</label><input class="wl-timeout" type="number" min="1" value="'+(data.timeout||300)+'"></div></div><div class="toolbar" style="margin-top:12px"><h4>Backends</h4><button class="btn btn-secondary btn-sm" type="button" onclick="addWorkloadBackend(this,null)">+ Add Backend</button></div><div class="wl-backends">'+((data.backends||[]).map(workloadBackendHTML).join('')||workloadBackendHTML(null))+'</div><div class="inline-err wl-error" style="display:none"></div><div style="margin-top:12px"><button class="btn btn-primary btn-sm">Save '+title+'</button></div></form>';}
function workloadBackendHTML(b){b=b||{};return '<div class="card wl-backend" data-original="'+escAttr(b.url||'')+'" data-clear-key="0" style="margin:8px 0"><div class="field-grid"><div class="field field-full"><label>Server URL</label><input class="wb-url" type="url" value="'+escAttr(b.url||'')+'" placeholder="http://server:8007/v1"></div><div class="field"><label>API key</label><div class="secret-row"><input class="wb-key" type="password" placeholder="'+escAttr(b.has_api_key?(b.api_key_mask+' — leave blank to keep'):'optional')+'"><button type="button" class="btn btn-danger btn-sm" onclick="clearWBKey(this)">Clear</button></div></div><div class="field"><label>Weight</label><input class="wb-weight" type="number" min="1" value="'+(b.weight||1)+'"></div><div class="field"><label>Max concurrent</label><input class="wb-max" type="number" min="0" value="'+(b.max_inflight||0)+'"></div><label class="field checkbox-row"><input class="wb-disabled" type="checkbox" '+(b.disabled?'checked':'')+'> Temporarily disabled</label></div><div style="display:flex;gap:8px;align-items:center"><button class="btn btn-secondary btn-sm" type="button" onclick="testAudioBackend(this)">Test health</button><button class="btn btn-danger btn-sm" type="button" onclick="this.closest(\'.wl-backend\').remove()">Remove</button><span class="wb-status mono"></span></div></div>';}
function addWorkloadBackend(btn,b){btn.closest('.workload-card').querySelector('.wl-backends').insertAdjacentHTML('beforeend',workloadBackendHTML(b));}
function clearWBKey(btn){var row=btn.closest('.wl-backend'),input=row.querySelector('.wb-key');input.value='';input.placeholder='will be cleared on save';row.dataset.clearKey='1';}
function collectWB(form){return Array.from(form.querySelectorAll('.wl-backend')).map(function(r){var b={url:r.querySelector('.wb-url').value.trim(),weight:parseInt(r.querySelector('.wb-weight').value,10)||1,max_inflight:parseInt(r.querySelector('.wb-max').value,10)||0,disabled:r.querySelector('.wb-disabled').checked};var k=r.querySelector('.wb-key').value;if(k)b.api_key=k;else if(r.dataset.clearKey==='1')b.api_key='';return b;});}
function testAudioBackend(btn){var form=btn.closest('.workload-card'),row=btn.closest('.wl-backend'),status=row.querySelector('.wb-status'),body={url:row.querySelector('.wb-url').value.trim(),path:'/health',section:form.dataset.kind},key=row.querySelector('.wb-key').value;if(key)body.api_key=key;status.textContent='Testing…';apiPost('/admin/workloads/probe',body).then(function(r){status.textContent=r.json.ok?'Healthy':'Failed (HTTP '+(r.json.status||'?')+')';status.style.color=r.json.ok?'var(--success)':'var(--danger)';});}
function saveAudio(e){e.preventDefault();var f=e.target,enabled=f.querySelector('.wl-enabled').checked,backs=collectWB(f),err=f.querySelector('.wl-error');var body={kind:f.dataset.kind,enabled:enabled,name:f.querySelector('.wl-name').value.trim(),model:f.querySelector('.wl-model').value.trim(),timeout:parseInt(f.querySelector('.wl-timeout').value,10)||300,backends:backs};if(enabled&&(!body.name||!backs.length||backs.some(function(b){return !b.url;}))){err.textContent='A model name and at least one backend URL are required.';err.style.display='block';return;}apiPost('/admin/audio/mutate',body).then(function(r){if(!r.ok){err.textContent=r.json.error&&r.json.error.message||'Save failed';err.style.display='block';return;}flash('Audio configuration saved','success');loadAudio();});}loadAudio();`
}

func documentsPageJS() string {
	return `var docState={};function loadDoc(){apiGet('/admin/documents/data').then(function(d){docState=d.paddleocr;renderDoc();});}
function renderDoc(){var d=docState||{};document.getElementById('documentEditor').innerHTML='<form id="docForm" class="card" onsubmit="saveDoc(event)"><div class="toolbar"><div><h3 style="margin:0">PaddleOCR Extraction</h3><p class="helper-text">Public route <code>/layout-parsing</code>; response remains the official PaddleOCR JSON shape.</p></div><label class="checkbox-row"><input id="docEnabled" type="checkbox" '+(docState?'checked':'')+'> Enabled</label></div><div class="field-grid"><div class="field"><label>Upstream endpoint</label><input id="docEndpoint" value="'+escAttr(d.endpoint||'/layout-parsing')+'"></div><div class="field"><label>Health endpoint</label><input id="docHealth" value="'+escAttr(d.health_endpoint||'/health')+'"></div><div class="field"><label>Timeout (seconds)</label><input id="docTimeout" type="number" min="1" value="'+(d.timeout||300)+'"></div></div><div class="toolbar" style="margin-top:12px"><h4>Backends</h4><button type="button" class="btn btn-secondary btn-sm" onclick="addDocBackend(null)">+ Add Backend</button></div><div id="docBackends"></div><div id="docErr" class="inline-err" style="display:none"></div><div style="margin-top:12px"><button class="btn btn-primary btn-sm">Save Documents</button></div></form>';(d.backends||[null]).forEach(addDocBackend);}
function addDocBackend(b){b=b||{};var row=document.createElement('div');row.className='card doc-backend';row.style.margin='8px 0';row.dataset.original=b.url||'';row.dataset.clearKey='0';row.innerHTML='<div class="field-grid"><div class="field field-full"><label>Server base URL</label><input class="db-url" type="url" value="'+escAttr(b.url||'')+'" placeholder="http://server:8002"></div><div class="field"><label>API key</label><div class="secret-row"><input class="db-key" type="password" placeholder="'+escAttr(b.has_api_key?(b.api_key_mask+' — leave blank to keep'):'optional')+'"><button type="button" class="btn btn-danger btn-sm" onclick="clearDBKey(this)">Clear</button></div></div><div class="field"><label>Weight</label><input class="db-weight" type="number" min="1" value="'+(b.weight||1)+'"></div><div class="field"><label>Max concurrent</label><input class="db-max" type="number" min="0" value="'+(b.max_inflight||0)+'"></div><label class="field checkbox-row"><input class="db-disabled" type="checkbox" '+(b.disabled?'checked':'')+'> Temporarily disabled</label></div><div style="display:flex;gap:8px"><button type="button" class="btn btn-secondary btn-sm" onclick="testDocBackend(this)">Test health</button><button type="button" class="btn btn-danger btn-sm" onclick="this.closest(\'.doc-backend\').remove()">Remove</button><span class="db-status mono"></span></div>';document.getElementById('docBackends').appendChild(row);}
function clearDBKey(btn){var row=btn.closest('.doc-backend'),input=row.querySelector('.db-key');input.value='';input.placeholder='will be cleared on save';row.dataset.clearKey='1';}
function testDocBackend(btn){var r=btn.closest('.doc-backend'),s=r.querySelector('.db-status'),body={url:r.querySelector('.db-url').value.trim(),path:document.getElementById('docHealth').value.trim(),section:'paddleocr'},k=r.querySelector('.db-key').value;if(k)body.api_key=k;s.textContent='Testing…';apiPost('/admin/workloads/probe',body).then(function(x){s.textContent=x.json.ok?'Healthy':'Failed (HTTP '+(x.json.status||'?')+')';s.style.color=x.json.ok?'var(--success)':'var(--danger)';});}
function saveDoc(e){e.preventDefault();var rows=Array.from(document.querySelectorAll('.doc-backend')),backs=rows.map(function(r){var b={url:r.querySelector('.db-url').value.trim(),weight:parseInt(r.querySelector('.db-weight').value,10)||1,max_inflight:parseInt(r.querySelector('.db-max').value,10)||0,disabled:r.querySelector('.db-disabled').checked},k=r.querySelector('.db-key').value;if(k)b.api_key=k;else if(r.dataset.clearKey==='1')b.api_key='';return b;}),body={enabled:document.getElementById('docEnabled').checked,endpoint:document.getElementById('docEndpoint').value.trim(),health_endpoint:document.getElementById('docHealth').value.trim(),timeout:parseInt(document.getElementById('docTimeout').value,10)||300,backends:backs},err=document.getElementById('docErr');if(body.enabled&&(!backs.length||backs.some(function(b){return !b.url;}))){err.textContent='At least one backend URL is required.';err.style.display='block';return;}apiPost('/admin/documents/mutate',body).then(function(r){if(!r.ok){err.textContent=r.json.error&&r.json.error.message||'Save failed';err.style.display='block';return;}flash('Document configuration saved','success');loadDoc();});}loadDoc();`
}
