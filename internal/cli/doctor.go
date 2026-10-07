package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nenych/chcli/internal/auth"
	"github.com/nenych/chcli/internal/chclient"
	"github.com/nenych/chcli/internal/repl"
	"github.com/nenych/chcli/internal/session"
)

func (a *app) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose a connection step by step",
		Long: `Check a connection one layer at a time: configuration, DNS, TCP, TLS,
OIDC discovery, the cached login session and finally ClickHouse itself.
Nothing that is printed contains credentials, and no browser is opened.`,
		Example: "  chcli doctor --profile production",
		Args:    noArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return a.doctor(cmd) },
	}
}

const doctorTimeout = 10 * time.Second

func (a *app) doctor(cmd *cobra.Command) error {
	failed := false
	report := func(name, detail string, err error) bool {
		if err != nil {
			failed = true
			fmt.Fprintf(a.stdout, "✗ %-18s %s\n", name, strings.ReplaceAll(session.FormatError(err), "\n", "\n"+strings.Repeat(" ", 21)))
			return false
		}
		fmt.Fprintf(a.stdout, "✓ %-18s %s\n", name, detail)
		return true
	}
	skip := func(name, reason string) { fmt.Fprintf(a.stdout, "- %-18s skipped: %s\n", name, reason) }

	_, resolved, err := a.resolve(cmd)
	if err != nil {
		report("Configuration", "", err)
		return &statusError{ExitUsage}
	}
	report("Configuration", fmt.Sprintf("%s, %s protocol, %s authentication", resolved.Addr(), resolved.Protocol, auth.Label(resolved.Auth.Type)), nil)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	opts := chclient.OptionsFrom(resolved, a.build.Version)

	// Network path: DNS, TCP, TLS. Each step depends on the previous one.
	reachable := a.doctorNetwork(ctx, opts, report, skip)

	provider, err := a.newProvider(resolved, false)
	if err != nil {
		return err
	}
	if sp, ok := provider.(auth.SessionProvider); ok {
		if discoverer, ok := provider.(interface {
			Endpoints(context.Context) (auth.Endpoints, error)
		}); ok {
			dctx, cancel := context.WithTimeout(ctx, doctorTimeout)
			ep, err := discoverer.Endpoints(dctx)
			cancel()
			detail := "token endpoint " + ep.Token
			if ep.Discovered {
				detail = "discovered from " + resolved.Auth.Issuer
			}
			report("OIDC endpoints", detail, err)
		}
		st, err := sp.Status()
		switch {
		case err != nil:
			report("OAuth session", "", err)
		case !st.LoggedIn:
			report("OAuth session", "", &auth.LoginRequiredError{Profile: resolved.Profile})
		case st.Expiry.IsZero() || time.Now().Before(st.Expiry):
			report("OAuth session", fmt.Sprintf("logged in as %s, token valid for %s", st.Identity, repl.FormatDuration(time.Until(st.Expiry))), nil)
		case st.Refreshable:
			report("OAuth session", fmt.Sprintf("logged in as %s, token expired and will be refreshed", st.Identity), nil)
		default:
			report("OAuth session", "", errors.New("the cached token has expired and cannot be refreshed; run chcli auth login"))
		}
	}

	if !reachable {
		skip("ClickHouse", "the server is not reachable")
	} else {
		client := chclient.New(opts, provider)
		cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
		err := client.Connect(cctx)
		cancel()
		info := client.Info()
		report("ClickHouse", fmt.Sprintf("version %s, authenticated as %s", info.Version, info.User), err)
		_ = client.Close()
	}

	if failed {
		return &statusError{ExitError}
	}
	return nil
}

func (a *app) doctorNetwork(ctx context.Context, opts chclient.Options,
	report func(name, detail string, err error) bool, skip func(name, reason string)) bool {
	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()

	if net.ParseIP(opts.Host) != nil {
		skip("DNS", "host is an IP address")
	} else {
		addrs, err := net.DefaultResolver.LookupHost(ctx, opts.Host)
		if !report("DNS", opts.Host+" resolves to "+strings.Join(addrs, ", "), err) {
			skip("TCP", "DNS resolution failed")
			return false
		}
	}

	started := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", opts.Addr())
	if !report("TCP", fmt.Sprintf("connected to %s in %s", opts.Addr(), time.Since(started).Round(100*time.Microsecond)), err) {
		return false
	}
	defer conn.Close()

	if !opts.Secure {
		skip("TLS", "the connection is not configured for TLS")
		return true
	}
	tlsConfig, err := opts.TLSConfig()
	if err != nil {
		return report("TLS", "", err)
	}
	tlsConn := tls.Client(conn, tlsConfig)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return report("TLS", "", err)
	}
	state := tlsConn.ConnectionState()
	detail := tls.VersionName(state.Version)
	if len(state.PeerCertificates) > 0 {
		cert := state.PeerCertificates[0]
		detail += fmt.Sprintf(", certificate for %s valid until %s", cert.Subject.CommonName, cert.NotAfter.Format("2006-01-02"))
	}
	if opts.InsecureSkipVerify {
		detail += " (certificate NOT verified)"
	}
	return report("TLS", detail, nil)
}
