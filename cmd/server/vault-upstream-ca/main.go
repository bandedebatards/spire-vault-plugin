package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/hcl"
	"github.com/spiffe/spire/pkg/common/pemutil"
	spi "github.com/spiffe/spire/proto/common/plugin"
	"github.com/spiffe/spire/proto/server/upstreamca"

	"github.com/zlabjp/spire-vault-plugin/pkg/vault"
)

const (
	CommonName = "spire-server"
)

// VaultPlugin implements UpstreamCA Plugin interface
type VaultPlugin struct {
	config *VaultPluginConfig
	vc     *vault.Client

	mu *sync.RWMutex
}

type VaultPluginConfig struct {
	// A URL of Vault server. (e.g., https://vault.example.com:8443/)
	VaultAddr string `hcl:"vault_addr"`
	// The method used for authentication to Vault.
	// The available methods are only 'token' and 'cert'.
	AuthMethod string `hcl:"auth_method"`
	// Name of mount point where TLS auth method is mounted. (e.g., /auth/<mount_point>/login)
	TLSAuthMountPoint string `hcl:"tls_auth_mount_point"`
	// Name of mount point where PKI secret engine is mounted. (e.g., /<mount_point>/ca/pem)
	PKIMountPoint string `hcl:"pki_mount_point"`
	// Configuration parameters to use when auth method is 'token'
	TokenAuthConfig VaultTokenAuthConfig `hcl:"token_auth_config"`
	// Configuration parameters to use when auth method is 'cert'
	CertAuthConfig VaultCertAuthConfig `hcl:"cert_auth_config"`
	// Path to a CA certificate file that the client verifies the server certificate.
	// PEM and DER format is supported.
	CACertPath string `hcl:"ca_cert_path"`
	// Request to issue a certificate with the specified TTL
	TTL string `hcl:"ttl"`
	// If true, vault client accepts any server certificates.
	// It should be used only test environment so on.
	TLSSkipVerify bool `hcl:"tls_skip_verify"`
}

// VaultTokenAuthConfig represents parameters for token auth method
type VaultTokenAuthConfig struct {
	// Token string to set into "X-Vault-Token" header
	Token string `hcl:"token"`
}

// VaultCertAuthConfig represents parameters for cert auth method
type VaultCertAuthConfig struct {
	// Path to a client certificate file.
	// PEM and DER format is supported.
	ClientCertPath string `hcl:"client_cert_path"`
	// Path to a client private key file.
	// PEM and DER format is supported.
	ClientKeyPath string `hcl:"client_key_path"`
}

const (
	pluginName = "vault"
)

func New() *VaultPlugin {
	return &VaultPlugin{
		mu: &sync.RWMutex{},
	}
}

func (p *VaultPlugin) Configure(ctx context.Context, req *spi.ConfigureRequest) (*spi.ConfigureResponse, error) {
	config := new(VaultPluginConfig)
	if err := hcl.Decode(config, req.Configuration); err != nil {
		return nil, fmt.Errorf("failed to decode configuration file: %v", err)
	}
	if errs := validatePluginConfig(config); len(errs) != 0 {
		return nil, errors.New(strings.Join(errs, "."))
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	am, err := vault.ParseAuthMethod(config.AuthMethod)
	if err != nil {
		return nil, err
	}

	vaultConfig := vault.New(am).WithEnvVar()
	cp := &vault.ClientParams{
		VaultAddr:         config.VaultAddr,
		CACertPath:        config.CACertPath,
		Token:             config.TokenAuthConfig.Token,
		TLSAuthMountPoint: config.TLSAuthMountPoint,
		PKIMountPoint:     config.PKIMountPoint,
		ClientKeyPath:     config.CertAuthConfig.ClientKeyPath,
		ClientCertPath:    config.CertAuthConfig.ClientCertPath,
		TTL:               config.TTL,
		TLSSKipVerify:     config.TLSSkipVerify,
	}
	if err := vaultConfig.SetClientParams(cp); err != nil {
		return nil, fmt.Errorf("failetd to prepare vault client")
	}

	vc, err := vaultConfig.NewAuthenticatedClient()
	if err != nil {
		return nil, fmt.Errorf("failed to prepare vault authentication: %v", err)
	}

	p.config = config
	p.vc = vc

	return &spi.ConfigureResponse{}, nil
}

func (p *VaultPlugin) SubmitCSR(ctx context.Context, req *upstreamca.SubmitCSRRequest) (*upstreamca.SubmitCSRResponse, error) {
	certReq := &pem.Block{Type: "CERTIFICATE REQUEST", Bytes: req.Csr}
	pemData := pem.EncodeToMemory(certReq)

	signResp, err := p.vc.SignIntermediate(CommonName, pemData)
	if err != nil {
		return nil, fmt.Errorf("SubmitCSR request is failed: %v", err)
	}
	if signResp == nil {
		return nil, errors.New("SubmitCSR response is empty")
	}

	// Parse PEM format data to get DER format data
	certificate, err := pemutil.ParseCertificate([]byte(signResp.CertPEM))
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %v", err)
	}

	// Combining CACertPEM and CACertChainPEM
	var bundles []*x509.Certificate
	if len(signResp.CACertChainPEM) != 0 {
		for i := range signResp.CACertChainPEM {
			c := signResp.CACertChainPEM[i]
			bundles, err = pemutil.ParseCertificates([]byte(c))
			if err != nil {
				return nil, fmt.Errorf("failed to parse upstream bundle certificates: %v", err)
			}
		}
	}
	caCertificates, err := pemutil.ParseCertificate([]byte(signResp.CACertPEM))
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %v", err)
	}
	bundles = append(bundles, caCertificates)

	var rawBundles []byte
	for i := range bundles {
		b := bundles[i]
		rawBundles = append(rawBundles, b.Raw...)
	}

	return &upstreamca.SubmitCSRResponse{
		Cert:                certificate.Raw,
		UpstreamTrustBundle: rawBundles,
	}, nil
}

func (p *VaultPlugin) GetPluginInfo(context.Context, *spi.GetPluginInfoRequest) (*spi.GetPluginInfoResponse, error) {
	return &spi.GetPluginInfoResponse{}, nil
}

// validatePluginConfig validates value of VaultPluginConfig
func validatePluginConfig(c *VaultPluginConfig) []string {
	var errs []string

	return errs
}

func main() {
	plugin.Serve(&plugin.ServeConfig{
		Plugins: map[string]plugin.Plugin{
			pluginName: upstreamca.GRPCPlugin{
				ServerImpl: &upstreamca.GRPCServer{
					Plugin: New(),
				},
			},
		},
		HandshakeConfig: upstreamca.Handshake,
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}