package models

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/grafana/grafana-aws-sdk/pkg/sigv4"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"

	"github.com/grafana/grafana/pkg/infra/httpclientprovider"
	"github.com/grafana/grafana/pkg/setting"
)

type proxyTransportCache struct {
	cache map[int64]cachedTransport
	sync.Mutex
}

// dataSourceTransport implements http.RoundTripper (https://golang.org/pkg/net/http/#RoundTripper)
type dataSourceTransport struct {
	datasourceName string
	headers        map[string]string
	transport      *http.Transport
	next           http.RoundTripper
}

// RoundTrip executes a single HTTP transaction, returning a Response for the provided Request.
func (d *dataSourceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for key, value := range d.headers {
		req.Header.Set(key, value)
	}

	// temporary solution to continue to support earlier supported metrics middleware.
	opts := &httpclient.Options{Labels: map[string]string{
		"datasource_name": d.datasourceName,
	}}
	rt := httpclientprovider.DataSourceMetricsMiddleware().CreateMiddleware(opts, d.next)

	return rt.RoundTrip(req)
}

type cachedTransport struct {
	updated time.Time

	*dataSourceTransport
}

var ptc = proxyTransportCache{
	cache: make(map[int64]cachedTransport),
}

func (ds *DataSource) getTimeout() time.Duration {
	timeout := 0
	if ds.JsonData != nil {
		timeout = ds.JsonData.Get("timeout").MustInt()
	}
	if timeout == 0 {
		timeout = setting.DataProxyTimeout
	}
	return time.Duration(timeout) * time.Second
}

func (ds *DataSource) GetHttpClient() (*http.Client, error) {
	transport, err := ds.GetHttpTransport()
	if err != nil {
		return nil, err
	}

	return &http.Client{
		Timeout:   ds.getTimeout(),
		Transport: transport,
	}, nil
}

// Creates a HTTP Transport middleware chain
func (ds *DataSource) GetHttpTransport() (*dataSourceTransport, error) {
	ptc.Lock()
	defer ptc.Unlock()

	if t, present := ptc.cache[ds.Id]; present && ds.Updated.Equal(t.updated) {
		return t.dataSourceTransport, nil
	}

	tlsConfig, err := ds.GetTLSConfig()
	if err != nil {
		return nil, err
	}

	tlsConfig.Renegotiation = tls.RenegotiateFreelyAsClient

	// Create transport which adds all
	customHeaders := ds.getCustomHeaders()
	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
		Proxy:           http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   ds.getTimeout(),
			KeepAlive: time.Duration(setting.DataProxyKeepAlive) * time.Second,
		}).Dial,
		TLSHandshakeTimeout:   time.Duration(setting.DataProxyTLSHandshakeTimeout) * time.Second,
		ExpectContinueTimeout: time.Duration(setting.DataProxyExpectContinueTimeout) * time.Second,
		MaxIdleConns:          setting.DataProxyMaxIdleConns,
		IdleConnTimeout:       time.Duration(setting.DataProxyIdleConnTimeout) * time.Second,
	}

	// Set default next round tripper to the default transport
	next := http.RoundTripper(transport)

	// Add SigV4 middleware if enabled, which will then defer to the default transport
	if ds.JsonData != nil && ds.JsonData.Get("sigV4Auth").MustBool() && setting.SigV4AuthEnabled {
		next = ds.sigV4Middleware(transport)
	}

	dsTransport := &dataSourceTransport{
		datasourceName: ds.Name,
		headers:        customHeaders,
		transport:      transport,
		next:           next,
	}

	ptc.cache[ds.Id] = cachedTransport{
		dataSourceTransport: dsTransport,
		updated:             ds.Updated,
	}

	return dsTransport, nil
}

type proxyTransportCache2 struct {
	cache map[int64]cachedRoundTripper2
	sync.Mutex
}

type cachedRoundTripper2 struct {
	updated      time.Time
	roundTripper http.RoundTripper
}

var ptc2 = proxyTransportCache2{
	cache: make(map[int64]cachedRoundTripper2),
}

func (ds *DataSource) GetHttpClient2(provider httpclient.Provider) (*http.Client, error) {
	transport, err := ds.GetHttpTransport2(provider)
	if err != nil {
		return nil, err
	}

	return &http.Client{
		Timeout:   ds.getTimeout(),
		Transport: transport,
	}, nil
}

func (ds *DataSource) GetHttpTransport2(provider httpclient.Provider) (http.RoundTripper, error) {
	ptc2.Lock()
	defer ptc2.Unlock()

	if t, present := ptc2.cache[ds.Id]; present && ds.Updated.Equal(t.updated) {
		return t.roundTripper, nil
	}

	rt, err := provider.GetTransport(ds.HTTPClientOptions())
	if err != nil {
		return nil, err
	}

	ptc2.cache[ds.Id] = cachedRoundTripper2{
		roundTripper: rt,
		updated:      ds.Updated,
	}

	return rt, nil
}

func (ds *DataSource) HTTPClientOptions() *httpclient.Options {
	opts := httpclient.Options{
		Timeouts: &httpclient.TimeoutOptions{
			Timeout:               ds.getTimeout(),
			KeepAlive:             time.Duration(setting.DataProxyKeepAlive) * time.Second,
			TLSHandshakeTimeout:   time.Duration(setting.DataProxyTLSHandshakeTimeout) * time.Second,
			ExpectContinueTimeout: time.Duration(setting.DataProxyExpectContinueTimeout) * time.Second,
			MaxIdleConns:          setting.DataProxyMaxIdleConns,
			IdleConnTimeout:       time.Duration(setting.DataProxyIdleConnTimeout) * time.Second,
		},
		Headers: ds.getCustomHeaders(),
		Labels: map[string]string{
			"datasource_name": ds.Name,
			"datasource_uid":  ds.Uid,
			"kind":            "datasource",
		},
		TLS: ds.TLSOptions(),
	}

	if ds.JsonData != nil {
		opts.CustomOptions = ds.JsonData.MustMap()
	}

	if ds.BasicAuth {
		opts.BasicAuth = &httpclient.BasicAuthOptions{
			User:     ds.BasicAuthUser,
			Password: ds.DecryptedBasicAuthPassword(),
		}
	} else if !ds.BasicAuth && ds.User != "" {
		opts.BasicAuth = &httpclient.BasicAuthOptions{
			User:     ds.User,
			Password: ds.DecryptedBasicAuthPassword(),
		}
	}

	if ds.JsonData != nil && ds.JsonData.Get("sigV4Auth").MustBool(false) {
		opts.SigV4 = &httpclient.SigV4Config{
			Service:       awsServiceNamespace(ds.Type),
			Region:        ds.JsonData.Get("sigV4Region").MustString(),
			AssumeRoleARN: ds.JsonData.Get("sigV4AssumeRoleArn").MustString(),
			AuthType:      ds.JsonData.Get("sigV4AuthType").MustString(),
			ExternalID:    ds.JsonData.Get("sigV4ExternalId").MustString(),
			Profile:       ds.JsonData.Get("sigV4Profile").MustString(),
		}

		if val, exists := ds.DecryptedValue("sigV4AccessKey"); exists {
			opts.SigV4.AccessKey = val
		}

		if val, exists := ds.DecryptedValue("sigV4SecretKey"); exists {
			opts.SigV4.SecretKey = val
		}
	}

	return &opts
}

func (ds *DataSource) TLSOptions() *httpclient.TLSOptions {
	var tlsSkipVerify, tlsClientAuth, tlsAuthWithCACert bool
	var serverName string

	if ds.JsonData != nil {
		tlsClientAuth = ds.JsonData.Get("tlsAuth").MustBool(false)
		tlsAuthWithCACert = ds.JsonData.Get("tlsAuthWithCACert").MustBool(false)
		tlsSkipVerify = ds.JsonData.Get("tlsSkipVerify").MustBool(false)
		serverName = ds.JsonData.Get("serverName").MustString()
	}

	opts := &httpclient.TLSOptions{
		InsecureSkipVerify: tlsSkipVerify,
		ServerName:         serverName,
	}

	if tlsClientAuth || tlsAuthWithCACert {
		if tlsAuthWithCACert {
			if val, exists := ds.DecryptedValue("tlsCACert"); exists && len(val) > 0 {
				opts.CACertificate = val
			}
		}

		if tlsClientAuth {
			if val, exists := ds.DecryptedValue("tlsClientCert"); exists && len(val) > 0 {
				opts.ClientCertificate = val
			}
			if val, exists := ds.DecryptedValue("tlsClientKey"); exists && len(val) > 0 {
				opts.ClientKey = val
			}
		}
	}

	return opts
}

func (ds *DataSource) sigV4Middleware(next http.RoundTripper) http.RoundTripper {
	decrypted := ds.DecryptedValues()

	return sigv4.New(
		&sigv4.Config{
			Service:       awsServiceNamespace(ds.Type),
			AccessKey:     decrypted["sigV4AccessKey"],
			SecretKey:     decrypted["sigV4SecretKey"],
			Region:        ds.JsonData.Get("sigV4Region").MustString(),
			AssumeRoleARN: ds.JsonData.Get("sigV4AssumeRoleArn").MustString(),
			AuthType:      ds.JsonData.Get("sigV4AuthType").MustString(),
			ExternalID:    ds.JsonData.Get("sigV4ExternalId").MustString(),
			Profile:       ds.JsonData.Get("sigV4Profile").MustString(),
		},
		next,
	)
}

func (ds *DataSource) GetTLSConfig() (*tls.Config, error) {
	var tlsSkipVerify, tlsClientAuth, tlsAuthWithCACert bool
	var serverName string

	if ds.JsonData != nil {
		tlsClientAuth = ds.JsonData.Get("tlsAuth").MustBool(false)
		tlsAuthWithCACert = ds.JsonData.Get("tlsAuthWithCACert").MustBool(false)
		tlsSkipVerify = ds.JsonData.Get("tlsSkipVerify").MustBool(false)
		serverName = ds.JsonData.Get("serverName").MustString()
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: tlsSkipVerify,
		ServerName:         serverName,
	}

	if tlsClientAuth || tlsAuthWithCACert {
		decrypted := ds.SecureJsonData.Decrypt()
		if tlsAuthWithCACert && len(decrypted["tlsCACert"]) > 0 {
			caPool := x509.NewCertPool()
			ok := caPool.AppendCertsFromPEM([]byte(decrypted["tlsCACert"]))
			if !ok {
				return nil, errors.New("failed to parse TLS CA PEM certificate")
			}
			tlsConfig.RootCAs = caPool
		}

		if tlsClientAuth {
			cert, err := tls.X509KeyPair([]byte(decrypted["tlsClientCert"]), []byte(decrypted["tlsClientKey"]))
			if err != nil {
				return nil, err
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
	}

	return tlsConfig, nil
}

func (ds *DataSource) GetTLSConfig2(httpClientProvider httpclient.Provider) (*tls.Config, error) {
	return httpClientProvider.GetTLSConfig(ds.TLSOptions())
}

// getCustomHeaders returns a map with all the to be set headers
// The map key represents the HeaderName and the value represents this header's value
func (ds *DataSource) getCustomHeaders() map[string]string {
	headers := make(map[string]string)
	if ds.JsonData == nil {
		return headers
	}

	decrypted := ds.SecureJsonData.Decrypt()
	index := 1
	for {
		headerNameSuffix := fmt.Sprintf("httpHeaderName%d", index)
		headerValueSuffix := fmt.Sprintf("httpHeaderValue%d", index)

		key := ds.JsonData.Get(headerNameSuffix).MustString()
		if key == "" {
			// No (more) header values are available
			break
		}

		if val, ok := decrypted[headerValueSuffix]; ok {
			headers[key] = val
		}
		index++
	}

	return headers
}

type cachedDecryptedJSON struct {
	updated time.Time
	json    map[string]string
}

type secureJSONDecryptionCache struct {
	cache map[int64]cachedDecryptedJSON
	sync.Mutex
}

var dsDecryptionCache = secureJSONDecryptionCache{
	cache: make(map[int64]cachedDecryptedJSON),
}

// DecryptedValues returns cached decrypted values from secureJsonData.
func (ds *DataSource) DecryptedValues() map[string]string {
	dsDecryptionCache.Lock()
	defer dsDecryptionCache.Unlock()

	if item, present := dsDecryptionCache.cache[ds.Id]; present && ds.Updated.Equal(item.updated) {
		return item.json
	}

	json := ds.SecureJsonData.Decrypt()
	dsDecryptionCache.cache[ds.Id] = cachedDecryptedJSON{
		updated: ds.Updated,
		json:    json,
	}

	return json
}

// DecryptedValue returns cached decrypted value from cached secureJsonData.
func (ds *DataSource) DecryptedValue(key string) (string, bool) {
	value, exists := ds.DecryptedValues()[key]
	return value, exists
}

// ClearDSDecryptionCache clears the datasource decryption cache.
func ClearDSDecryptionCache() {
	dsDecryptionCache.Lock()
	defer dsDecryptionCache.Unlock()

	dsDecryptionCache.cache = make(map[int64]cachedDecryptedJSON)
}

func awsServiceNamespace(dsType string) string {
	switch dsType {
	case DS_ES, DS_ES_OPEN_DISTRO:
		return "es"
	case DS_PROMETHEUS:
		return "aps"
	default:
		panic(fmt.Sprintf("Unsupported datasource %q", dsType))
	}
}
