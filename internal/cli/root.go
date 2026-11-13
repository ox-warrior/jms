package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/leiyupeng/jumpserver-cli/internal/assetcache"
	"github.com/leiyupeng/jumpserver-cli/internal/config"
	"github.com/leiyupeng/jumpserver-cli/internal/googlelogin"
	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
	"github.com/leiyupeng/jumpserver-cli/internal/tui"
	"github.com/leiyupeng/jumpserver-cli/internal/version"
)

// defaultServer and defaultJmsdAddr are empty in the source tree: both name a
// specific deployment, and a repository cannot know which one. Every user
// supplies them through jms.example.yaml (or --server / --jmsd-addr); see
// requireAddresses below for what happens when they do not.
//
// They are vars rather than consts so a deployment that wants a zero-config
// install can bake its own values in at build time without patching source:
//
//	go build -ldflags "-X ...internal/cli.defaultServer=jumpserver.example.com \
//	                   -X ...internal/cli.defaultJmsdAddr=http://jmsd.example.com:8391"
//
// Whatever gets injected must be a public hostname, not an internal IP: an IP
// only resolves from inside one network, and when it does not the failure
// surfaces as "dial tcp <ip>:2222: i/o timeout", which reads like Koko is down
// rather than like the address was never reachable from here.
var defaultServer = ""

// jmsd terminates plain HTTP (see cmd/jmsd, which calls ListenAndServe with no
// certificate), so an http:// jmsd_addr carries connection tickets in the clear
// -- fine on a trusted network, worth a TLS proxy in front before it crosses
// anything wider.
var defaultJmsdAddr = ""

// googleClientID and googleClientSecret are the Google OAuth "Desktop app"
// client jms signs in with. Empty in the source tree and injected by the
// Makefile, like the addresses above: a public repository cannot carry one
// company's client. A build without them cannot sign in at all (see
// errNoGoogleClient).
var (
	googleClientID     = ""
	googleClientSecret = ""
)

// googleClient is the compiled-in client, or the zero value when there is
// none.
var errNoGoogleClient = errors.New("this jms was built without a Google client ID and cannot sign in -- build it with make")

func googleClient() googlelogin.Client {
	return googlelogin.Client{ID: googleClientID, Secret: googleClientSecret}
}

// requireAddresses rejects a run that has no JumpServer host or no jmsd base
// URL. Both are empty in a stock build (see defaultServer), so the common first
// run is a user who has not written a config file yet -- the message names the
// exact file to create rather than only stating what is missing.
// jmsdOnly is a run that never dials Koko -- "-w", "ls", "login": demanding
// --server there would refuse a mode that has no use for it.
func requireAddresses(server, jmsdAddr, cfgPath string, jmsdOnly bool) error {
	var missing []string
	if !jmsdOnly && strings.TrimSpace(server) == "" {
		missing = append(missing, "server (--server)")
	}
	if strings.TrimSpace(jmsdAddr) == "" {
		missing = append(missing, "jmsd_addr (--jmsd-addr)")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%s not set: put them in %s (see jms.example.yaml) or pass the flags",
		strings.Join(missing, " and "), cfgPath)
}

type rootFlags struct {
	server     string
	sshPort    int
	identity   string
	jmsdAddr   string
	theme      string
	cacheTTL   time.Duration
	k9sPath    string
	logFile    string
	configPath string
	// command is "-c TYPE [NAME]" (docs/PLAN.md §8.1), mfmc's own shortcut
	// syntax. Empty means the ordinary TUI launch.
	command string
	// tunnelPort is "-w PORT": serve SOCKS5 on that loopback port and carry
	// every stream to jmsd. Zero means no tunnel, which is the normal run.
	tunnelPort int
	// account pins which account a headless "-c TYPE NAME" connects as, by
	// Username (not Alias -- see resolveAccount). Only meaningful alongside a
	// name-bearing command; rejected on its own so a typo'd or forgotten "-c"
	// does not silently connect as the wrong default account.
	account string
	// remoteCommand is everything after "--", run on the asset instead of
	// opening a shell. Not a flag: filled from the positional arguments.
	remoteCommand string
	// commandTimeout bounds remoteCommand; 0 disables it.
	commandTimeout time.Duration
	// onBehalfOf is who a service key (-i) is acting for, recorded in jmsd's
	// audit. For a gateway that runs jms for many people under one key.
	onBehalfOf string
}

func Execute() error {
	return newRootCommand().Execute()
}

// runEnv is what every jms mode resolves before it talks to anything: the
// client config file and the two addresses it may supply.
type runEnv struct {
	cfg      config.ClientConfig
	cfgPath  string
	server   string
	jmsdAddr string
}

// loadEnv reads the client config file and resolves both addresses.
func loadEnv(cmd *cobra.Command, flags *rootFlags) runEnv {
	env := runEnv{cfgPath: flags.configPath}
	if env.cfgPath == "" {
		if p, err := config.DefaultClientConfigPath(); err == nil {
			env.cfgPath = p
		}
	}
	// A missing file is silent (all defaults). Anything else -- a YAML typo,
	// a bad mode -- is not: it also falls back to defaults, but silently doing
	// so meant one stray character put every setting in the file back to its
	// built-in value with nothing said. jms then ran against the wrong jmsd,
	// which reads as jmsd being broken.
	cfg, err := config.LoadClientConfig(env.cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jms: ignoring %s: %v\n", env.cfgPath, err)
	}
	env.cfg = cfg

	// Addresses resolve flag > config file > built-in default. The flag is
	// only allowed to win when it was actually typed: comparing against the
	// default value cannot tell "not passed" from "passed the same value as
	// the default", and Changed can. cmd.Flags() rather than
	// PersistentFlags() so a subcommand sees the root's flags too.
	env.server = flags.server
	if !cmd.Flags().Changed("server") && cfg.Server != "" {
		env.server = cfg.Server
	}
	env.jmsdAddr = flags.jmsdAddr
	if !cmd.Flags().Changed("jmsd-addr") && cfg.JmsdAddr != "" {
		env.jmsdAddr = cfg.JmsdAddr
	}
	return env
}

// open refuses a run missing an address it needs, then opens the log. The
// returned func closes the log; it is safe to call when the log failed to
// open.
//
// quiet drops the "logging to" line, for runs whose stderr is read by a
// program -- an AI agent running a remote command sees that line on every
// call, mixed into the command's own stderr.
func (env runEnv) open(flags *rootFlags, needServer, quiet bool) (func(), error) {
	// Checked here rather than at each use: ResolveServer rejects an empty
	// host on the headless path, but the TUI path never calls it and would
	// open a full-screen UI that fails on the first request with the
	// terminal already taken. One guard covers both.
	if err := requireAddresses(env.server, env.jmsdAddr, env.cfgPath, !needServer); err != nil {
		return nil, resolveErr(err)
	}

	debug := env.cfg.LogLevel == "debug"
	logFile := flags.logFile
	if logFile == "" {
		logFile = env.cfg.LogFile
	}
	// Opened here, before the TUI takes the screen, because this is the last
	// moment a warning can reach the user's terminal. A failure is reported
	// and then ignored: jms is still perfectly usable without a log, and
	// refusing to start over an unwritable file would be worse than the
	// problem.
	path, err := jmslog.Open(logFile, debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jms: logging disabled: %v\n", err)
		return func() {}, nil
	}
	jmslog.Printf("---- jms %s starting (server=%s jmsd=%s debug=%t) ----",
		version.Version, env.server, env.jmsdAddr, debug)
	if !quiet {
		fmt.Fprintf(os.Stderr, "jms: logging to %s\n", path)
	}
	return func() { _ = jmslog.Close() }, nil
}

// prepare is loadEnv and open together, for the subcommands, which have no
// update check to start in between.
func prepare(cmd *cobra.Command, flags *rootFlags, needServer bool) (runEnv, func(), error) {
	env := loadEnv(cmd, flags)
	closeLog, err := env.open(flags, needServer, true)
	return env, closeLog, err
}

// validateOnBehalfOf: only a service key may speak for someone (jmsd refuses
// it otherwise), and the tunnel's streams are audited without it.
func validateOnBehalfOf(flags rootFlags) error {
	switch {
	case flags.onBehalfOf == "":
		return nil
	case flags.identity == "":
		return errors.New("--on-behalf-of needs -i: only a service key can act for someone else")
	case flags.tunnelPort != 0:
		return errors.New("--on-behalf-of is not supported with -w")
	}
	return nil
}

// validateTunnelFlags rejects a "-w" that cannot mean anything, alongside
// parseCommandFlags doing the same for "--account". Split out rather than
// inlined in RunE because each of these is a mistake a user makes once and
// would otherwise cost them a silent no-op: "-w 9090 -c ec2 ng0" would connect
// to the asset and exit, with no proxy ever started and nothing to say so.
func validateTunnelFlags(flags rootFlags) error {
	if flags.tunnelPort == 0 {
		return nil
	}
	if flags.command != "" {
		return errors.New("-w and -c cannot be combined: -w opens a proxy that stays up, " +
			"-c connects to one asset and exits")
	}
	if flags.tunnelPort < 1 || flags.tunnelPort > 65535 {
		return fmt.Errorf("-w %d: port out of range", flags.tunnelPort)
	}
	return nil
}

func newRootCommand() *cobra.Command {
	var flags rootFlags
	cmd := &cobra.Command{
		Use:           "jms",
		Short:         "K9s-style JumpServer client",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Cobra already refuses positional arguments here, but only as a side
		// effect of a subcommand being registered (its legacyArgs check) --
		// drop "version" and every stray argument would start being silently
		// ignored instead. Pinning it explicitly makes the guarantee stand on
		// its own, and lets the message name the mistake that actually produces
		// one: an unquoted shortcut, where `jms -c ec2 ng0` leaves "ng0" here.
		//
		// The one exception is everything after "--", which is a remote
		// command (ArgsLenAtDash is 0 exactly when every argument came after
		// it). Cobra also stops looking for subcommand names at "--", so
		// `jms -c "ec2 x" -- login` runs login remotely rather than "jms login".
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || cmd.ArgsLenAtDash() == 0 {
				return nil
			}
			return resolveErr(fmt.Errorf(
				"unexpected argument %q for %q -- a shortcut has to be one quoted value, as in: "+
					"jms -c \"ec2 name\"; a remote command goes after \"--\", as in: jms -c \"ec2 name\" -- uptime",
				args[0], cmd.CommandPath()))
		},
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			// Everything after "--" is the remote command, joined the way ssh
			// joins it: one string, interpreted by the remote shell.
			flags.remoteCommand = strings.Join(args, " ")
			// Deferred so it covers every exit below, flag errors included:
			// once a remote command is given, any code but 255 is the
			// command's own.
			if flags.remoteCommand != "" {
				defer func() { err = asCommandFailure(err) }()
			}
			// --account only means something alongside a name-bearing "-c" --
			// checked before anything else runs so a typo'd or forgotten "-c"
			// fails loudly instead of silently ignoring --account and falling
			// back to the default account (docs/PLAN.md §8.1).
			if err := validateTunnelFlags(flags); err != nil {
				return resolveErr(err)
			}
			if err := validateOnBehalfOf(flags); err != nil {
				return resolveErr(err)
			}
			target, err := parseCommandFlags(flags)
			if err != nil {
				return resolveErr(err)
			}

			env := loadEnv(cmd, &flags)
			// Started here and joined once the log is open, so the round trip
			// overlaps that work; an unreachable CDN costs updateCheckBudget once.
			updates := startUpdateCheck(env.cfg.UpdateURL)

			closeLog, err := env.open(&flags, flags.tunnelPort == 0, flags.remoteCommand != "")
			if err != nil {
				return err
			}
			defer closeLog()
			// Before every mode, scripted ones included: see update.go.
			if release := <-updates; release != nil {
				return resolveErr(forceUpdate(cmd.Context(), release))
			}
			server, jmsdAddr := env.server, env.jmsdAddr

			// Placed after the log is open (and before any Koko address is
			// resolved) so a tunnel's stream failures land in the same file as
			// everything else -- it is a long-running mode, and its log is the
			// client-side half of jmsd's own per-stream lines.
			if flags.tunnelPort != 0 {
				return runTunnel(cmd.Context(), flags.tunnelPort, jmsdAddr, flags.identity)
			}

			// target.Name != "" is the only branch that dials anything itself;
			// the other two only ever hand Server/SSHPort to tui.Run, which
			// resolves them on its own.
			if target != nil && target.Name != "" {
				endpoints, err := config.ResolveServer(server, flags.sshPort)
				if err != nil {
					return resolveErr(err)
				}
				return runHeadless(cmd.Context(), headlessTarget{
					Kind: target.Kind, Name: target.Name, Account: flags.account,
					Command: flags.remoteCommand, Timeout: flags.commandTimeout,
				}, headlessDeps{
					Endpoints: endpoints, JmsdAddr: jmsdAddr,
					Identity: flags.identity, OnBehalfOf: flags.onBehalfOf, K9sPath: strings.TrimSpace(flags.k9sPath),
					CacheTTL: flags.cacheTTL,
				})
			}

			opts := tui.Options{
				Server: server, SSHPort: flags.sshPort,
				Identity: flags.identity, JmsdAddr: jmsdAddr, Theme: flags.theme,
				CacheTTL: flags.cacheTTL, K9sPath: strings.TrimSpace(flags.k9sPath),
				Google: googleClient(),
			}
			if target != nil {
				// tui.ResourceKind is jmsd.ResourceKind (a type alias, see
				// internal/tui/resources.go), so target.Kind assigns directly.
				opts.InitialKind = target.Kind
			}

			return tui.Run(opts)
		},
	}
	// Cobra appends its own "(default ...)" for any flag with a non-zero
	// default, so these usage strings only mention the config file -- spelling
	// the default out here too would print it twice on one line.
	cmd.PersistentFlags().StringVar(&flags.server, "server", defaultServer,
		"JumpServer host to reach Koko on; the config file's server can set this")
	cmd.PersistentFlags().IntVar(&flags.sshPort, "ssh-port", 2222, "Koko SSH port")
	cmd.PersistentFlags().StringVarP(&flags.identity, "identity", "i", "", "private key path")
	cmd.PersistentFlags().StringVar(&flags.onBehalfOf, "on-behalf-of", "",
		"with -i: the person this service key is acting for, recorded in jmsd's audit")
	cmd.PersistentFlags().StringVar(&flags.jmsdAddr, "jmsd-addr", defaultJmsdAddr,
		"jmsd base URL; the config file's jmsd_addr can set this")
	cmd.PersistentFlags().StringVar(&flags.theme, "theme", tui.DefaultTheme,
		"colour theme: "+strings.Join(tui.ThemeNames(), ", "))
	cmd.PersistentFlags().DurationVar(&flags.cacheTTL, "cache-ttl", assetcache.DefaultTTL,
		"reuse the locally cached asset listing for this long (0 disables it; r always refreshes)")
	cmd.PersistentFlags().StringVar(&flags.k9sPath, "k9s-path", "",
		"path to the local k9s binary (default: look up \"k9s\" on PATH)")
	cmd.PersistentFlags().StringVar(&flags.logFile, "log-file", "",
		"where to write jms's log (default: from config file, or "+defaultLogPath()+")")
	cmd.PersistentFlags().StringVar(&flags.configPath, "config", "",
		"path to jms's YAML config file (default: "+defaultConfigPath()+")")
	cmd.PersistentFlags().StringVarP(&flags.command, "command", "c", "",
		"shortcut: \"type\" to open the TUI pre-filtered, or \"type name\" to connect directly "+
			"(type is one of "+strings.Join(jmsd.ResourceKindTokens(), ", ")+"; matching is exact)")
	cmd.PersistentFlags().IntVarP(&flags.tunnelPort, "tunnel", "w", 0,
		"serve a SOCKS5 proxy on 127.0.0.1:PORT, tunnelled through jmsd (needs tunnel.enabled on jmsd)")
	cmd.PersistentFlags().StringVar(&flags.account, "account", "",
		"account username to connect as with -c \"type name\" (default: the asset's first permitted account)")

	cmd.PersistentFlags().DurationVar(&flags.commandTimeout, "timeout", 60*time.Second,
		"with -c \"type name\" -- command: give up after this long (0 = never); output is also capped at 1MiB")

	cmd.AddCommand(newVersionCommand(), newLoginCommand(&flags))
	return cmd
}

// defaultLogPath is only for the flag's help text, so an unavailable cache
// directory just leaves the help slightly vaguer rather than failing.
func defaultLogPath() string {
	path, err := jmslog.DefaultPath()
	if err != nil {
		return "a jms.log in your cache dir"
	}
	return path
}

// defaultConfigPath is only for the flag's help text, so an unavailable
// config directory just leaves the help slightly vaguer rather than failing.
func defaultConfigPath() string {
	path, err := config.DefaultClientConfigPath()
	if err != nil {
		return "~/.config/jms/config.yaml"
	}
	return path
}

// newLoginCommand is how a human refreshes MFA on behalf of the runs that
// will not prompt: "ls" and "-c ... -- CMD", usually driven by an AI agent.
// It is just the sign-in those runs do, allowed to ask. When MFA is still
// fresh nothing is asked, and jmsd does not report when it will lapse, so
// "still valid" is all there is to say.
func newLoginCommand(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Sign in with Google now, so commands that never prompt (-c ... -- CMD) keep working",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, closeLog, err := prepare(cmd, flags, false)
			if err != nil {
				return err
			}
			defer closeLog()
			_, identity, err := signIn(cmd.Context(), flags.identity, flags.onBehalfOf, env.jmsdAddr, true)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "jms: signed in as %s\n", identity.Username)
			return nil
		},
	}
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print version and compatibility baseline",
		Run: func(_ *cobra.Command, _ []string) {
			// jms's own version on its own line; the JumpServer/Koko release
			// demoted to what it actually is -- a note about what this was
			// tested against, not a third version of equal standing. Printed
			// as one indented line so the two cannot be misread as a set.
			fmt.Printf("jms %s\n  tested against JumpServer / Koko %s\n",
				version.Version, version.JumpServerBaseline)
		},
	}
}
