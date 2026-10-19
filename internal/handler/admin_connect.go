package handler

import (
	"context"
	"net/http"
	"time"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

// usesConnect reports whether the config involves an Eidonix connect hub: a
// connect: block, or any backend written as connect://.
func usesConnect(cfg *config.Config) bool {
	if cfg.Connect != nil {
		return true
	}
	any := func(backends []config.BackendConfig) bool {
		for _, b := range backends {
			if httputil.IsConnectURL(b.URL) {
				return true
			}
		}
		return false
	}
	for i := range cfg.Models {
		if any(cfg.EffectiveBackends(&cfg.Models[i])) {
			return true
		}
	}
	if a := cfg.Audio.Whisper; a != nil && any(a.Backends) {
		return true
	}
	if a := cfg.Audio.TTS; a != nil && any(a.Backends) {
		return true
	}
	if d := cfg.Documents.PaddleOCR; d != nil && any(d.Backends) {
		return true
	}
	return false
}

// ConnectData serves the read-only "Connected services" panel: the hub's
// catalog (site, online, service, models, healthy). It answers
// {"configured": false} when no connect hub is in use, so the panel stays
// hidden for deployments that don't use one.
func (h *AdminHandler) ConnectData(w http.ResponseWriter, r *http.Request) {
	cfg := h.cs.Get()
	if !usesConnect(cfg) {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	hubURL, application := cfg.ConnectSettings()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	cat, err := config.FetchHubCatalog(ctx, httputil.NewHTTPClient(), hubURL, application, "")
	out := map[string]any{"configured": true, "url": hubURL}
	if err != nil {
		out["error"] = err.Error()
	} else {
		sites := make([]map[string]any, 0, len(cat.Sites))
		for _, s := range cat.Sites {
			services := make([]map[string]any, 0, len(s.Services))
			for _, svc := range s.Services {
				models := svc.Models
				if models == nil {
					models = []string{}
				}
				services = append(services, map[string]any{
					"name": svc.Name, "kind": svc.Kind, "healthy": svc.Healthy, "models": models,
				})
			}
			sites = append(sites, map[string]any{"name": s.Name, "online": s.Online, "services": services})
		}
		out["sites"] = sites
	}
	writeJSON(w, http.StatusOK, out)
}

const connectCardHTML = `<div class="card" id="connectCard" style="display:none">
  <div class="card-header"><h2>Connected services</h2><span class="hint" id="connectHub"></span></div>
  <p class="helper-text" style="margin:0 0 10px">Services offered by remote sites through the Eidonix connect hub. Use one as a backend with <span class="mono">connect://&lt;site&gt;/&lt;service&gt;/&lt;path&gt;</span>.</p>
  <div class="table-wrap"><table class="data-table">
    <thead><tr><th>Site</th><th style="width:90px">Online</th><th>Service</th><th>Models</th><th style="width:90px">Healthy</th></tr></thead>
    <tbody id="connectBody"><tr><td colspan="5" class="empty-cell">Loading…</td></tr></tbody>
  </table></div>
</div>`

const connectPanelJS = `
function loadConnect(){apiGet('/admin/connect/data').then(function(d){
  var card=document.getElementById('connectCard');if(!d.configured){card.style.display='none';return;}
  card.style.display='';document.getElementById('connectHub').textContent=d.url||'';
  var body=document.getElementById('connectBody');
  if(d.error){body.innerHTML='<tr><td colspan="5" class="empty-cell" style="color:var(--red)">'+esc(d.error)+'</td></tr>';return;}
  var dot=function(ok,t){return '<span class="health-dot '+(ok?'health-online':'health-offline')+'" title="'+t+'"></span>'+t;};
  var rows=[];(d.sites||[]).forEach(function(s){var svcs=s.services||[];
    if(!svcs.length){rows.push('<tr><td><strong>'+esc(s.name)+'</strong></td><td>'+dot(s.online,s.online?'yes':'no')+'</td><td colspan="3" class="empty-cell">no services</td></tr>');return;}
    svcs.forEach(function(v,i){rows.push('<tr><td>'+(i?'':'<strong>'+esc(s.name)+'</strong>')+'</td><td>'+(i?'':dot(s.online,s.online?'yes':'no'))+'</td><td class="mono">'+esc(v.name)+' <span class="hint">'+esc(v.kind||'')+'</span></td><td class="mono">'+esc((v.models||[]).join(', '))+'</td><td>'+dot(v.healthy,v.healthy?'yes':'no')+'</td></tr>');});});
  body.innerHTML=rows.length?rows.join(''):'<tr><td colspan="5" class="empty-cell">No sites in the catalog</td></tr>';
}).catch(function(){});}
loadConnect();`
