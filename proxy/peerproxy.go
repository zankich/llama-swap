package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/proxy/config"
)

type peerProxyMember struct {
	peerID       string
	reverseProxy *httputil.ReverseProxy
	apiKey       string
	modelRewrite string
}

type PeerProxy struct {
	peers    config.PeerDictionaryConfig
	proxyMap map[string]*peerProxyMember
}

func NewPeerProxy(peers config.PeerDictionaryConfig, proxyLogger *logmon.Monitor) (*PeerProxy, error) {
	proxyMap := make(map[string]*peerProxyMember)

	// Sort peer IDs for consistent iteration order
	peerIDs := make([]string, 0, len(peers))
	for peerID := range peers {
		peerIDs = append(peerIDs, peerID)
	}
	sort.Strings(peerIDs)

	for _, peerID := range peerIDs {
		peer := peers[peerID]

		// Create a transport with per-peer timeout configuration
		peerTransport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   time.Duration(peer.Timeouts.Connect) * time.Second,
				KeepAlive: time.Duration(peer.Timeouts.KeepAlive) * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   time.Duration(peer.Timeouts.TLSHandshake) * time.Second,
			ResponseHeaderTimeout: time.Duration(peer.Timeouts.ResponseHeader) * time.Second,
			ExpectContinueTimeout: time.Duration(peer.Timeouts.ExpectContinue) * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       time.Duration(peer.Timeouts.IdleConn) * time.Second,
		}

		// Resolve base URL: use proxyBaseURL if set, otherwise default to <proxy>/v1
		baseURL := peer.ProxyBaseURLParsed
		if baseURL == nil {
			baseURL = &url.URL{
				Scheme: peer.ProxyURL.Scheme,
				Host:   peer.ProxyURL.Host,
				Path:   peer.ProxyURL.Path + "/v1",
			}
		}
		reverseProxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = baseURL.Scheme
				req.URL.Host = baseURL.Host
				// Strip /v1 prefix from incoming path and prepend base path
				path := strings.TrimPrefix(req.URL.Path, "/v1")
				req.URL.Path = baseURL.Path + path
				req.URL.RawQuery = req.URL.RawQuery
				req.Host = baseURL.Host
			},
			Transport: peerTransport,
		}

		reverseProxy.ModifyResponse = func(resp *http.Response) error {
			if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				resp.Header.Set("X-Accel-Buffering", "no")
			}
			return nil
		}

		reverseProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			proxyLogger.Warnf("peer %s: proxy error: %v", peerID, err)
			errMsg := fmt.Sprintf("peer proxy error: %v", err)
			if runtime.GOOS == "darwin" && strings.Contains(err.Error(), "connect: no route to host") {
				errMsg += " (hint: on macOS, check System Settings > Privacy & Security > Local Network permissions)"
			}
			http.Error(w, errMsg, http.StatusBadGateway)
		}

		pp := &peerProxyMember{
			peerID:       peerID,
			reverseProxy: reverseProxy,
			apiKey:       peer.ApiKey,
		}

		// Map each model to this peer's proxy
		for _, modelID := range peer.Models {
			if _, found := proxyMap[modelID]; found {
				proxyLogger.Warnf("peer %s: model %s already mapped to another peer, skipping", peerID, modelID)
				continue
			}
			proxyMap[modelID] = pp
		}

		// Map each alias to this peer's proxy
		for aliasName, canonicalModel := range peer.Alias {
			if _, found := proxyMap[aliasName]; found {
				proxyLogger.Warnf("peer %s: alias %s already mapped to another peer, skipping", peerID, aliasName)
				continue
			}
			proxyMap[aliasName] = &peerProxyMember{
				peerID:       pp.peerID,
				reverseProxy: pp.reverseProxy,
				apiKey:       pp.apiKey,
				modelRewrite: canonicalModel,
			}
		}
	}

	return &PeerProxy{
		peers:    peers,
		proxyMap: proxyMap,
	}, nil
}

func (p *PeerProxy) HasPeerModel(modelID string) bool {
	_, found := p.proxyMap[modelID]
	return found
}

// GetPeerFilters returns the filters for a peer model, or empty filters if not found
func (p *PeerProxy) GetPeerFilters(modelID string) config.Filters {
	pp, found := p.proxyMap[modelID]
	if !found {
		return config.Filters{}
	}
	// Get the peer config using the peerID
	peer, found := p.peers[pp.peerID]
	if !found {
		return config.Filters{}
	}
	return peer.Filters
}

func (p *PeerProxy) GetModelRewrite(modelID string) string {
	pp, found := p.proxyMap[modelID]
	if !found {
		return ""
	}
	return pp.modelRewrite
}

func (p *PeerProxy) ListPeers() config.PeerDictionaryConfig {
	return p.peers
}

func (p *PeerProxy) ProxyRequest(model_id string, writer http.ResponseWriter, request *http.Request) error {
	pp, found := p.proxyMap[model_id]
	if !found {
		return fmt.Errorf("no peer proxy found for model %s", model_id)
	}

	// Inject API key if configured for this peer
	if pp.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+pp.apiKey)
		request.Header.Set("x-api-key", pp.apiKey)
	}

	pp.reverseProxy.ServeHTTP(writer, request)
	return nil
}
