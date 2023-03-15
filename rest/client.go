package rest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

var ErrAppUpdateRequired = errors.New( "application update required" )
var ErrHttpStatusBad = errors.New( "bad HTTP status" )
var ErrBadPin = errors.New( "bad public key PIN" )

type Config struct {
	APIVersion				string			`yaml:"-"`										// Current API version is 1.0.0
	Host					string			`yaml:"host,omitempty"`							// FQDN of the server
	Port					int				`yaml:"port,omitempty"`							// Port to connect to when issuing REST requests
	Domain					string			`yaml:"-"`										// Domain ( hide.me )
	AccessTokenFile			string			`yaml:"accessToken,omitempty"`					// Access-Token for REST requests
	Username				string			`yaml:"username,omitempty"`						// Username ( Access-Token takes precedence )
	Password				string			`yaml:"password,omitempty"`						// Password ( Access-Token takes precedence )
	RestTimeout				time.Duration	`yaml:"restTimeout,omitempty"`					// Timeout for REST requests
	ReconnectWait			time.Duration	`yaml:"reconnectWait,omitempty"`				// Reconnect timeout
	AccessTokenUpdateDelay	time.Duration	`yaml:"AccessTokenUpdateDelay,omitempty"`		// Period to wait for when updating a stale Access-Token
	CA						string			`yaml:"CA,omitempty"`							// CA certificate bundle ( empty for system-wide CA roots )
	FirewallMark			int				`yaml:"firewallMark,omitempty"`					// Firewall mark for the traffic generated by this app
	DnsServers				string			`yaml:"dnsServers,omitempty"`					// DNS servers to use when resolving names for client requests ( wireguard link uses it's assigned DNS servers )
	Filter					Filter			`yaml:"filter,omitempty"`						// Filtering settings
}

type Client struct {
	*Config
	
	client					*http.Client
	transport				*http.Transport
	resolver				*net.Resolver
	dnsServers				[]string
	remote					*net.TCPAddr
	
	accessToken				[]byte
	authorizedPins			map[string]string
}

func NewClient( config *Config ) ( c *Client, err error ) {
	c = &Client{ Config: config }
	if c.Config.Port == 0 { c.Config.Port = 432 }
	c.transport = &http.Transport{
		DialContext:			c.dialContext,
		TLSHandshakeTimeout:	time.Second * 5,
		DisableKeepAlives:		true,
		ResponseHeaderTimeout:	time.Second * 5,
		ForceAttemptHTTP2:		true,
	}
	c.transport.TLSClientConfig = &tls.Config{
		NextProtos:				[]string{ "h2" },
		ServerName:				"",
		MinVersion:				tls.VersionTLS13,
		VerifyPeerCertificate:	c.Pins,
	}
	if len( config.CA ) > 0 {
		pem, err := os.ReadFile( config.CA )
		if err != nil { return nil, err }
		c.transport.TLSClientConfig.RootCAs = x509.NewCertPool()
		ok := c.transport.TLSClientConfig.RootCAs.AppendCertsFromPEM( pem )
		if ! ok { return nil, errors.New( "Bad certificate in " + config.CA ) }
	}
	c.client = &http.Client{
		Transport:	c.transport,
		Timeout:	c.Config.RestTimeout,
	}
	c.resolver = &net.Resolver{ PreferGo: true, Dial: c.dialContext }
	if len( c.Config.DnsServers ) > 0 {
		for _, dnsServer := range strings.Split( c.Config.DnsServers, "," ) {
			c.dnsServers = append( c.dnsServers, strings.TrimSpace( dnsServer ) )
		}
	} else { c.dnsServers = append( c.dnsServers, "1.1.1.1:53" ) }
	if len( config.AccessTokenFile ) > 0 {
		accessTokenBytes, acErr := ioutil.ReadFile( config.AccessTokenFile )
		if acErr == nil { c.accessToken, _ = base64.StdEncoding.DecodeString( string( accessTokenBytes ) ) }
	}
	
	c.authorizedPins = map[string]string{
		"Hide.Me Root CA": "AdKh8rXi68jeqv5kEzF4wJ9M2R89gFuMILRQ1uwADQI=",
		"Hide.Me Server CA #1": "CsEyDelMHMPh9qLGgeQn8sJwdUwvc+fCMhOU9Ne5PbU=",
		"DigiCert Global Root CA": "r/mIkG3eEpVdm+u/ko/cwxzOMo1bk4TyHIlByibiA5E=",
		"DigiCert TLS RSA SHA256 2020 CA1": "RQeZkB42znUfsDIIFWIRiYEcKl7nHwNFwWCrnMMJbVc=",
	}
	return
}

func ( c *Client ) Remote() *net.TCPAddr { return c.remote }

// Pins checks public key pins of authorized hide.me/hideservers.net CA certificates
func ( c *Client ) Pins( _ [][]byte, verifiedChains [][]*x509.Certificate) error {
	for _, chain := range verifiedChains {
		chainLoop:
		for _, certificate := range chain {
			if !certificate.IsCA { continue }
			sum := sha256.Sum256( certificate.RawSubjectPublicKeyInfo )
			pin := base64.StdEncoding.EncodeToString( sum[:] )
			for name, authorizedPin := range c.authorizedPins {
				if certificate.Subject.CommonName == name && pin == authorizedPin {
					fmt.Println( "Pins:", certificate.Subject.CommonName, "pin OK" )
					continue chainLoop
				}
			}
			fmt.Println( "Pins:", certificate.Subject.CommonName, "pin failed" )
			return ErrBadPin
		}
	}
	return nil
}

// Custom dialContext to set the socket mark on sockets or dial DNS servers
func ( c *Client ) dialContext( ctx context.Context, network, addr string ) ( net.Conn, error ) {
	dialer := &net.Dialer{}
	if c.Config.FirewallMark > 0 {
		dialer.Control = func( _, _ string, rawConn syscall.RawConn ) ( err error ) {
			_ = rawConn.Control( func( fd uintptr ) {
				err = syscall.SetsockoptInt( int(fd), unix.SOL_SOCKET, unix.SO_MARK, c.Config.FirewallMark )
				if err != nil { fmt.Println( "Dial: [ERR] Set mark failed,", err ) }
			})
			return
		}
	}
	if network == "udp" { addr = c.dnsServers[ rand.Intn( len( c.dnsServers ) ) ] }
	return dialer.DialContext( ctx, network, addr )
}

func ( c *Client ) postJson( ctx context.Context, url string, object interface{} ) ( responseBody []byte, err error ) {
	body, err := json.MarshalIndent( object, "", "\t" )
	if err != nil { return }
	connectCtx, cancel := context.WithTimeout( ctx, c.Config.RestTimeout )
	defer cancel()
	request, err := http.NewRequestWithContext( connectCtx, "POST", url, bytes.NewReader( body ) )
	if err != nil { return }
	request.Header.Set( "user-agent", "HIDE.ME.LINUX.CLI-0.9.3")
	request.Header.Add( "content-type", "application/json")
	response, err := c.client.Do( request )
	if err != nil { return }
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden { fmt.Println( "Rest: [ERR] Application update required" ); return nil, ErrAppUpdateRequired }
	if response.StatusCode != http.StatusOK { fmt.Println( "Rest: [ERR] Bad HTTP response (", response.StatusCode, ")" ); err = ErrHttpStatusBad; return }
	return io.ReadAll( response.Body )
}

func ( c *Client ) HaveAccessToken() bool { if c.accessToken != nil { return true }; return false }

// Resolve resolves an IP of a Hide.me endpoint and stores that IP for further use. Hide.me balances DNS rapidly, so once an IP is acquired it needs to be used for the remainder of the session
func ( c *Client ) Resolve( ctx context.Context ) ( err error ) {
	if ip := net.ParseIP( c.Config.Host ); ip != nil {											// c.Host is an IP address, allow that
		c.remote = &net.TCPAddr{ IP: ip, Port: c.Config.Port }									// Set remote endpoint to that IP
		c.transport.TLSClientConfig.ServerName = "hideservers.net"								// any.hideservers.net is always a certificate SAN
		return
	}
	lookupCtx, cancel := context.WithTimeout( ctx, time.Second * 5 )
	defer cancel()
	addrs, err := c.resolver.LookupIPAddr( lookupCtx, c.Config.Host )							// If DNS fails during reconnect then the remote server address in c.remote will be reused for the reconnection attempt
	if err != nil {																				// that's cool, but far from optimal
		fmt.Println( "Resolve: [ERR]", c.Config.Host, "lookup failed,", err )
		if c.remote != nil { fmt.Println( "Resolve: Using previous lookup response", c.remote.String() ); return nil }
		return
	}
	if len( addrs ) == 0 { return errors.New( "dns lookup failed for " + c.Config.Host ) }
	if addrs[0].IP == nil { return errors.New( "no IP found for " + c.Config.Host ) }
	c.transport.TLSClientConfig.ServerName = c.Config.Host
	c.remote = &net.TCPAddr{ IP: addrs[0].IP, Port: c.Config.Port }
	fmt.Println( "Name: Resolved", c.Config.Host, "to", c.remote.IP )
	return
}

// Connect issues a connect request to a Hide.me "Connect" endpoint which expects an ordinary POST request with a ConnectRequest JSON payload
func ( c *Client ) Connect( ctx context.Context, key wgtypes.Key ) ( connectResponse *ConnectResponse, err error ) {
	connectRequest := &ConnectRequest{
		Host:			strings.TrimSuffix( c.Config.Host, ".hideservers.net" ),
		Domain:			c.Config.Domain,
		AccessToken:	c.accessToken,
		PublicKey:		key[:],
	}
	if err = connectRequest.Check(); err != nil { return }
	
	responseBody, err := c.postJson( ctx, "https://" + c.remote.String() + "/" + c.Config.APIVersion + "/connect", connectRequest )
	if err != nil { return }
	
	connectResponse = &ConnectResponse{}
	err = json.Unmarshal( responseBody, connectResponse )
	return
}

// Disconnect issues a disconnect request to a Hide.me "Disconnect" endpoint which expects an ordinary POST request with a DisconnectRequest JSON payload
func ( c *Client ) Disconnect( sessionToken []byte ) ( err error ) {
	disconnectRequest := &DisconnectRequest{
		Host:			strings.TrimSuffix( c.Config.Host, ".hideservers.net" ),
		Domain:			c.Config.Domain,
		SessionToken:	sessionToken,
	}
	if err = disconnectRequest.Check(); err != nil { return }
	
	_, err = c.postJson( context.Background(), "https://" + c.remote.String() + "/" + c.Config.APIVersion + "/disconnect", disconnectRequest )
	return
}

// GetAccessToken issues an AccessToken request to a Hide.me "AccessToken" endpoint which expects an ordinary POST request with a AccessTokenRequest JSON payload
func ( c *Client ) GetAccessToken( ctx context.Context ) ( err error ) {
	accessTokenRequest := &AccessTokenRequest{
		Host:			strings.TrimSuffix( c.Config.Host, ".hideservers.net" ),
		Domain:			c.Config.Domain,
		AccessToken:	c.accessToken,
		Username:		c.Config.Username,
		Password:		c.Config.Password,
	}
	if err = accessTokenRequest.Check(); err != nil { return }
	
	accessTokenJson, err := c.postJson( ctx, "https://" + c.remote.String() + "/" + c.Config.APIVersion + "/accessToken", accessTokenRequest )
	if err != nil { return }
	
	accessTokenString := ""
	if err = json.Unmarshal( accessTokenJson, &accessTokenString ); err != nil { return }
	if c.accessToken, err = base64.StdEncoding.DecodeString( accessTokenString ); err != nil { return }
	
	if len( c.Config.AccessTokenFile ) > 0 { err = ioutil.WriteFile( c.Config.AccessTokenFile, []byte( accessTokenString ), 0600 ) }
	return
}

func ( c *Client ) ApplyFilter() ( err error ) {
	if err = c.Config.Filter.Check(); err != nil { return }
	response, err := c.postJson( context.Background(), "https://vpn.hide.me:4321/filter", c.Config.Filter )
	if string(response) == "false" { err = errors.New( "filter failed" ) }
	return
}