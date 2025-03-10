/*
Copyright 2014 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// git-sync is a command that pulls a git repository to a local directory.

package main // import "k8s.io/git-sync/cmd/git-sync"

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"
	"golang.org/x/sys/unix"
	"k8s.io/git-sync/pkg/cmd"
	"k8s.io/git-sync/pkg/hook"
	"k8s.io/git-sync/pkg/logging"
	"k8s.io/git-sync/pkg/pid1"
	"k8s.io/git-sync/pkg/version"
)

var flVersion = pflag.Bool("version", false, "print the version and exit")
var flHelp = pflag.BoolP("help", "h", false, "print help text and exit")
var flManual = pflag.Bool("man", false, "print the full manual and exit")

var flVerbose = pflag.IntP("verbose", "v", 0,
	"logs at this V level and lower will be printed")

var flRepo = pflag.String("repo", envString("GIT_SYNC_REPO", ""),
	"the git repository to clone (required)")
var flBranch = pflag.String("branch", envString("GIT_SYNC_BRANCH", ""),
	"the git branch to check out (defaults to the default branch of --repo)")
var flRev = pflag.String("rev", envString("GIT_SYNC_REV", "HEAD"),
	"the git revision (tag or hash) to check out")
var flDepth = pflag.Int("depth", envInt("GIT_SYNC_DEPTH", 0),
	"create a shallow clone with history truncated to the specified number of commits")
var flSubmodules = pflag.String("submodules", envString("GIT_SYNC_SUBMODULES", "recursive"),
	"git submodule behavior: one of 'recursive', 'shallow', or 'off'")

var flRoot = pflag.String("root", envString("GIT_SYNC_ROOT", ""),
	"the root directory for git-sync operations (required)")
var flLink = pflag.String("link", envString("GIT_SYNC_LINK", ""),
	"the path (absolute or relative to --root) at which to create a symlink to the directory holding the checked-out files (defaults to the leaf dir of --repo)")
var flErrorFile = pflag.String("error-file", envString("GIT_SYNC_ERROR_FILE", ""),
	"the path (absolute or relative to --root) to an optional file into which errors will be written (defaults to disabled)")
var flPeriod = pflag.Duration("period", envDuration("GIT_SYNC_PERIOD", 10*time.Second),
	"how long to wait between syncs, must be >= 10ms; --wait overrides this")
var flSyncTimeout = pflag.Duration("sync-timeout", envDuration("GIT_SYNC_SYNC_TIMEOUT", 120*time.Second),
	"the total time allowed for one complete sync, must be >= 10ms; --timeout overrides this")
var flOneTime = pflag.Bool("one-time", envBool("GIT_SYNC_ONE_TIME", false),
	"exit after the first sync")
var flSyncOnSignal = pflag.String("sync-on-signal", envString("GIT_SYNC_SYNC_ON_SIGNAL", ""),
	"sync on receipt of the specified signal (e.g. SIGHUP)")
var flMaxFailures = pflag.Int("max-failures", envInt("GIT_SYNC_MAX_FAILURES", 0),
	"the number of consecutive failures allowed before aborting (the first sync must succeed, -1 will retry forever")
var flChmod = pflag.Int("change-permissions", envInt("GIT_SYNC_PERMISSIONS", 0),
	"optionally change permissions on the checked-out files to the specified mode")

var flTouchFile = pflag.String("touch-file", envString("GIT_SYNC_TOUCH_FILE", ""),
	"the path (absolute or relative to --root) to an optional file which will be touched whenever a sync completes (defaults to disabled)")

var flSparseCheckoutFile = pflag.String("sparse-checkout-file", envString("GIT_SYNC_SPARSE_CHECKOUT_FILE", ""),
	"the path to a sparse-checkout file")

var flExechookCommand = pflag.String("exechook-command", envString("GIT_SYNC_EXECHOOK_COMMAND", ""),
	"an optional command to be run when syncs complete")
var flExechookTimeout = pflag.Duration("exechook-timeout", envDuration("GIT_SYNC_EXECHOOK_TIMEOUT", time.Second*30),
	"the timeout for the exechook")
var flExechookBackoff = pflag.Duration("exechook-backoff", envDuration("GIT_SYNC_EXECHOOK_BACKOFF", time.Second*3),
	"the time to wait before retrying a failed exechook")

var flWebhookURL = pflag.String("webhook-url", envString("GIT_SYNC_WEBHOOK_URL", ""),
	"a URL for optional webhook notifications when syncs complete")
var flWebhookMethod = pflag.String("webhook-method", envString("GIT_SYNC_WEBHOOK_METHOD", "POST"),
	"the HTTP method for the webhook")
var flWebhookStatusSuccess = pflag.Int("webhook-success-status", envInt("GIT_SYNC_WEBHOOK_SUCCESS_STATUS", 200),
	"the HTTP status code indicating a successful webhook (-1 disables success checks")
var flWebhookTimeout = pflag.Duration("webhook-timeout", envDuration("GIT_SYNC_WEBHOOK_TIMEOUT", time.Second),
	"the timeout for the webhook")
var flWebhookBackoff = pflag.Duration("webhook-backoff", envDuration("GIT_SYNC_WEBHOOK_BACKOFF", time.Second*3),
	"the time to wait before retrying a failed webhook")

var flUsername = pflag.String("username", envString("GIT_SYNC_USERNAME", ""),
	"the username to use for git auth")
var flPassword = pflag.String("password", envString("GIT_SYNC_PASSWORD", ""),
	"the password or personal access token to use for git auth (prefer --password-file or this env var)")
var flPasswordFile = pflag.String("password-file", envString("GIT_SYNC_PASSWORD_FILE", ""),
	"the file from which the password or personal access token for git auth will be sourced")

var flSSH = pflag.Bool("ssh", envBool("GIT_SYNC_SSH", false),
	"use SSH for git operations")
var flSSHKeyFile = pflag.String("ssh-key-file", envMultiString([]string{"GIT_SYNC_SSH_KEY_FILE", "GIT_SSH_KEY_FILE"}, "/etc/git-secret/ssh"),
	"the SSH key to use")
var flSSHKnownHosts = pflag.Bool("ssh-known-hosts", envMultiBool([]string{"GIT_SYNC_KNOWN_HOSTS", "GIT_KNOWN_HOSTS"}, true),
	"enable SSH known_hosts verification")
var flSSHKnownHostsFile = pflag.String("ssh-known-hosts-file", envMultiString([]string{"GIT_SYNC_SSH_KNOWN_HOSTS_FILE", "GIT_SSH_KNOWN_HOSTS_FILE"}, "/etc/git-secret/known_hosts"),
	"the known_hosts file to use")
var flAddUser = pflag.Bool("add-user", envBool("GIT_SYNC_ADD_USER", false),
	"add a record to /etc/passwd for the current UID/GID (needed to use SSH with an arbitrary UID)")

var flCookieFile = pflag.Bool("cookie-file", envMultiBool([]string{"GIT_SYNC_COOKIE_FILE", "GIT_COOKIE_FILE"}, false),
	"use a git cookiefile (/etc/git-secret/cookie_file) for authentication")

var flAskPassURL = pflag.String("askpass-url", envMultiString([]string{"GIT_SYNC_ASKPASS_URL", "GIT_ASKPASS_URL"}, ""),
	"a URL to query for git credentials (username=<value> and password=<value>)")

var flGitCmd = pflag.String("git", envString("GIT_SYNC_GIT", "git"),
	"the git command to run (subject to PATH search, mostly for testing)")
var flGitConfig = pflag.String("git-config", envString("GIT_SYNC_GIT_CONFIG", ""),
	"additional git config options in 'section.var1:val1,\"section.sub.var2\":\"val2\"' format")
var flGitGC = pflag.String("git-gc", envString("GIT_SYNC_GIT_GC", "auto"),
	"git garbage collection behavior: one of 'auto', 'always', 'aggressive', or 'off'")

var flHTTPBind = pflag.String("http-bind", envString("GIT_SYNC_HTTP_BIND", ""),
	"the bind address (including port) for git-sync's HTTP endpoint")
var flHTTPMetrics = pflag.Bool("http-metrics", envBool("GIT_SYNC_HTTP_METRICS", true),
	"enable metrics on git-sync's HTTP endpoint")
var flHTTPprof = pflag.Bool("http-pprof", envBool("GIT_SYNC_HTTP_PPROF", false),
	"enable the pprof debug endpoints on git-sync's HTTP endpoint")

// Obsolete flags, kept for compat.
var flWait = pflag.Float64("wait", envFloat("GIT_SYNC_WAIT", 0),
	"DEPRECATED: use --period instead")
var flTimeout = pflag.Int("timeout", envInt("GIT_SYNC_TIMEOUT", 0),
	"DEPRECATED: use --sync-timeout instead")
var flDest = pflag.String("dest", envString("GIT_SYNC_DEST", ""),
	"DEPRECATED: use --link instead")
var flSyncHookCommand = pflag.String("sync-hook-command", envString("GIT_SYNC_HOOK_COMMAND", ""),
	"DEPRECATED: use --exechook-command instead")
var flMaxSyncFailures = pflag.Int("max-sync-failures", envInt("GIT_SYNC_MAX_SYNC_FAILURES", 0),
	"DEPRECATED: use --max-failures instead")

func init() {
	pflag.CommandLine.MarkDeprecated("wait", "use --period instead")
	pflag.CommandLine.MarkDeprecated("timeout", "use --sync-timeout instead")
	pflag.CommandLine.MarkDeprecated("dest", "use --link instead")
	pflag.CommandLine.MarkDeprecated("sync-hook-command", "use --exechook-command instead")
	pflag.CommandLine.MarkDeprecated("max-sync-failures", "use --max-failures instead")
}

// Total pull/error, summary on pull duration
var (
	// TODO: have a marker for "which" servergroup
	syncDuration = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "git_sync_duration_seconds",
		Help: "Summary of git_sync durations",
	}, []string{"status"})

	syncCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "git_sync_count_total",
		Help: "How many git syncs completed, partitioned by state (success, error, noop)",
	}, []string{"status"})

	askpassCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "git_sync_askpass_calls",
		Help: "How many git askpass calls completed, partitioned by state (success, error)",
	}, []string{"status"})
)

const (
	metricKeySuccess = "success"
	metricKeyError   = "error"
	metricKeyNoOp    = "noop"
)

type submodulesMode string

const (
	submodulesRecursive submodulesMode = "recursive"
	submodulesShallow   submodulesMode = "shallow"
	submodulesOff       submodulesMode = "off"
)

type gcMode string

const (
	gcAuto       = "auto"
	gcAlways     = "always"
	gcAggressive = "aggressive"
	gcOff        = "off"
)

func init() {
	prometheus.MustRegister(syncDuration)
	prometheus.MustRegister(syncCount)
	prometheus.MustRegister(askpassCount)
}

func envString(key, def string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return def
}

func envMultiString(keys []string, def string) string {
	for i, key := range keys {
		if val := os.Getenv(key); val != "" {
			if i != 0 {
				fmt.Fprintf(os.Stderr, "Env %s has been deprecated, use %s instead\n", key, keys[0])
			}
			return val
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if val := os.Getenv(key); val != "" {
		parsed, err := strconv.ParseBool(val)
		if err == nil {
			return parsed
		}
		fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid bool env %s=%s: %v\n", key, val, err)
	}
	return def
}

func envMultiBool(keys []string, def bool) bool {
	for i, key := range keys {
		if val := os.Getenv(key); val != "" {
			parsed, err := strconv.ParseBool(val)
			if err == nil {
				if i != 0 {
					fmt.Fprintf(os.Stderr, "Env %s has been deprecated, use %s instead\n", key, keys[0])
				}
				return parsed
			}
			fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid bool env %s=%s: %v\n", key, val, err)
		}
	}
	return def
}

func envInt(key string, def int) int {
	if val := os.Getenv(key); val != "" {
		parsed, err := strconv.ParseInt(val, 0, 0)
		if err == nil {
			return int(parsed)
		}
		fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid int env %s=%s: %v\n", key, val, err)
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if val := os.Getenv(key); val != "" {
		parsed, err := strconv.ParseFloat(val, 64)
		if err == nil {
			return parsed
		}
		fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid float env %s=%s: %v\n", key, val, err)
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if val := os.Getenv(key); val != "" {
		parsed, err := time.ParseDuration(val)
		if err == nil {
			return parsed
		}
		fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid duration env %s=%s: %v\n", key, val, err)
	}
	return def
}

// repoSync represents the remote repo and the local sync of it.
type repoSync struct {
	cmd        string         // the git command to run
	root       string         // absolute path to the root directory
	repo       string         // remote repo to sync
	branch     string         // remote branch to sync
	rev        string         // the rev or SHA to sync
	depth      int            // for shallow sync
	submodules submodulesMode // how to handle submodules
	gc         gcMode         // garbage collection
	chmod      int            // mode to change repo to, or 0
	link       string         // the name of the symlink to publish under `root`
	authURL    string         // a URL to re-fetch credentials, or ""
	sparseFile string         // path to a sparse-checkout file
	log        *logging.Logger
	run        *cmd.Runner
}

func main() {
	// In case we come up as pid 1, act as init.
	if os.Getpid() == 1 {
		fmt.Fprintf(os.Stderr, "INFO: detected pid 1, running init handler\n")
		code, err := pid1.ReRun()
		if err == nil {
			os.Exit(code)
		}
		fmt.Fprintf(os.Stderr, "ERROR: unhandled pid1 error: %v\n", err)
		os.Exit(127)
	}

	//
	// Parse and verify flags.  Errors here are fatal.
	//

	pflag.Parse()

	// Handle print-and-exit cases.
	if *flVersion {
		fmt.Println(version.VERSION)
		os.Exit(0)
	}
	if *flHelp {
		pflag.CommandLine.SetOutput(os.Stdout)
		pflag.PrintDefaults()
		os.Exit(0)
	}
	if *flManual {
		printManPage()
		os.Exit(0)
	}

	// Init logging very early, so most errors can be written to a file.
	log := func() *logging.Logger {
		if strings.HasPrefix(*flErrorFile, ".") {
			fmt.Fprintf(os.Stderr, "ERROR: --error-file may not start with '.'")
			os.Exit(1)
		}
		dir, file := filepath.Split(makeAbsPath(*flErrorFile, *flRoot))
		return logging.New(dir, file, *flVerbose)
	}()
	cmdRunner := cmd.NewRunner(log)

	if *flRepo == "" {
		handleConfigError(log, true, "ERROR: --repo must be specified")
	}

	if *flDepth < 0 { // 0 means "no limit"
		handleConfigError(log, true, "ERROR: --depth must be greater than or equal to 0")
	}

	switch submodulesMode(*flSubmodules) {
	case submodulesRecursive, submodulesShallow, submodulesOff:
	default:
		handleConfigError(log, true, "ERROR: --submodules must be one of %q, %q, or %q", submodulesRecursive, submodulesShallow, submodulesOff)
	}

	switch *flGitGC {
	case gcAuto, gcAlways, gcAggressive, gcOff:
	default:
		handleConfigError(log, true, "ERROR: --git-gc must be one of %q, %q, %q, or %q", gcAuto, gcAlways, gcAggressive, gcOff)
	}

	if *flRoot == "" {
		handleConfigError(log, true, "ERROR: --root must be specified")
	}

	if *flDest != "" {
		// Back-compat
		*flLink = *flDest
	}
	if *flLink == "" {
		parts := strings.Split(strings.Trim(*flRepo, "/"), "/")
		*flLink = parts[len(parts)-1]
	}
	if strings.HasPrefix(filepath.Base(*flLink), ".") {
		handleConfigError(log, true, "ERROR: --link must not start with '.'")
	}

	if *flWait != 0 {
		// Back-compat
		*flPeriod = time.Duration(int(*flWait*1000)) * time.Millisecond
	}
	if *flPeriod < 10*time.Millisecond {
		handleConfigError(log, true, "ERROR: --period must be at least 10ms")
	}

	var syncSig syscall.Signal
	if *flSyncOnSignal != "" {
		if num, err := strconv.ParseInt(*flSyncOnSignal, 0, 0); err == nil {
			// sync-on-signal value is a number
			syncSig = syscall.Signal(num)
		} else {
			// sync-on-signal value is a name
			syncSig = unix.SignalNum(*flSyncOnSignal)
			if syncSig == 0 {
				// last resort - maybe they said "HUP", meaning "SIGHUP"
				syncSig = unix.SignalNum("SIG" + *flSyncOnSignal)
			}
		}
		if syncSig == 0 {
			handleConfigError(log, true, "ERROR: --sync-on-signal must be a valid signal name or number")
		}
	}

	if *flTimeout != 0 {
		// Back-compat
		*flSyncTimeout = time.Duration(*flTimeout) * time.Second
	}
	if *flSyncTimeout < 10*time.Millisecond {
		handleConfigError(log, true, "ERROR: --sync-timeout must be at least 10ms")
	}

	if *flMaxSyncFailures != 0 {
		// Back-compat
		*flMaxFailures = *flMaxSyncFailures
	}

	if *flTouchFile != "" {
		if strings.HasPrefix(*flTouchFile, ".") {
			handleConfigError(log, true, "ERROR: --touch-file may not start with '.'")
		}
	}
	absTouchFile := makeAbsPath(*flTouchFile, *flRoot)

	if *flSyncHookCommand != "" {
		// Back-compat
		*flExechookCommand = *flSyncHookCommand
	}
	if *flExechookCommand != "" {
		if *flExechookTimeout < time.Second {
			handleConfigError(log, true, "ERROR: --exechook-timeout must be at least 1s")
		}
		if *flExechookBackoff < time.Second {
			handleConfigError(log, true, "ERROR: --exechook-backoff must be at least 1s")
		}
	}

	if *flWebhookURL != "" {
		if *flWebhookStatusSuccess < -1 {
			handleConfigError(log, true, "ERROR: --webhook-success-status must be a valid HTTP code or -1")
		}
		if *flWebhookTimeout < time.Second {
			handleConfigError(log, true, "ERROR: --webhook-timeout must be at least 1s")
		}
		if *flWebhookBackoff < time.Second {
			handleConfigError(log, true, "ERROR: --webhook-backoff must be at least 1s")
		}
	}

	if *flPassword != "" && *flPasswordFile != "" {
		handleConfigError(log, true, "ERROR: only one of --password and --password-file may be specified")
	}
	if *flUsername != "" {
		if *flPassword == "" && *flPasswordFile == "" {
			handleConfigError(log, true, "ERROR: --password or --password-file must be set when --username is specified")
		}
	}

	if *flSSH {
		if *flUsername != "" {
			handleConfigError(log, true, "ERROR: only one of --ssh and --username may be specified")
		}
		if *flPassword != "" {
			handleConfigError(log, true, "ERROR: only one of --ssh and --password may be specified")
		}
		if *flPasswordFile != "" {
			handleConfigError(log, true, "ERROR: only one of --ssh and --password-file may be specified")
		}
		if *flAskPassURL != "" {
			handleConfigError(log, true, "ERROR: only one of --ssh and --askpass-url may be specified")
		}
		if *flCookieFile {
			handleConfigError(log, true, "ERROR: only one of --ssh and --cookie-file may be specified")
		}
		if *flSSHKeyFile == "" {
			handleConfigError(log, true, "ERROR: --ssh-key-file must be specified when --ssh is specified")
		}
		if *flSSHKnownHosts {
			if *flSSHKnownHostsFile == "" {
				handleConfigError(log, true, "ERROR: --ssh-known-hosts-file must be specified when --ssh-known-hosts is specified")
			}
		}
	}

	// From here on, output goes through logging.
	log.V(0).Info("starting up",
		"pid", os.Getpid(),
		"uid", os.Getuid(),
		"gid", os.Getgid(),
		"home", os.Getenv("HOME"),
		"args", logSafeArgs(os.Args),
		"env", logSafeEnv(os.Environ()))

	if _, err := exec.LookPath(*flGitCmd); err != nil {
		log.Error(err, "ERROR: git executable not found", "git", *flGitCmd)
		os.Exit(1)
	}

	// Make sure the root exists.  0755 ensures that this is usable as a volume
	// when the consumer isn't running as the same UID.  We do this very early
	// so that we can normalize the path even when there are symlinks in play.
	if err := os.MkdirAll(*flRoot, 0755); err != nil {
		log.Error(err, "ERROR: can't make root dir", "path", *flRoot)
		os.Exit(1)
	}
	absRoot, err := normalizePath(*flRoot)
	if err != nil {
		log.Error(err, "ERROR: can't normalize root path", "path", *flRoot)
		os.Exit(1)
	}
	if absRoot != *flRoot {
		log.V(0).Info("normalized root path", "root", *flRoot, "result", absRoot)
	}

	// Convert the link into an absolute path.  We don't want to mkdir here,
	// since it may be under --root, and that confuses `git clone`.
	// TODO(thockin): put repo in a subdir and mkdir + nortmalizePath() here
	absLink := makeAbsPath(*flLink, absRoot)

	if *flAddUser {
		if err := addUser(); err != nil {
			log.Error(err, "ERROR: can't add user")
			os.Exit(1)
		}
	}

	// Capture the various git parameters.
	git := &repoSync{
		cmd:        *flGitCmd,
		root:       absRoot,
		repo:       *flRepo,
		branch:     *flBranch,
		rev:        *flRev,
		depth:      *flDepth,
		submodules: submodulesMode(*flSubmodules),
		gc:         gcMode(*flGitGC),
		chmod:      *flChmod,
		link:       absLink,
		authURL:    *flAskPassURL,
		sparseFile: *flSparseCheckoutFile,
		log:        log,
		run:        cmdRunner,
	}

	// This context is used only for git credentials initialization. There are
	// no long-running operations like `git clone`, so hopefully 30 seconds will be enough.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	// Set various configs we want, but users might override.
	if err := git.setupDefaultGitConfigs(ctx); err != nil {
		log.Error(err, "can't set default git configs")
		os.Exit(1)
	}

	if *flUsername != "" {
		if *flPasswordFile != "" {
			passwordFileBytes, err := os.ReadFile(*flPasswordFile)
			if err != nil {
				log.Error(err, "can't read password file", "file", *flPasswordFile)
				os.Exit(1)
			}
			*flPassword = string(passwordFileBytes)
		}
	}

	if *flSSH {
		if err := git.SetupGitSSH(*flSSHKnownHosts, *flSSHKeyFile, *flSSHKnownHostsFile); err != nil {
			log.Error(err, "can't set up git SSH", "keyFile", *flSSHKeyFile, "knownHosts", *flSSHKnownHosts, "knownHostsFile", *flSSHKnownHostsFile)
			os.Exit(1)
		}
	}

	if *flCookieFile {
		if err := git.SetupCookieFile(ctx); err != nil {
			log.Error(err, "can't set up git cookie file")
			os.Exit(1)
		}
	}

	// This needs to be after all other git-related config flags.
	if *flGitConfig != "" {
		if err := git.setupExtraGitConfigs(ctx, *flGitConfig); err != nil {
			log.Error(err, "can't set additional git configs", "configs", *flGitConfig)
			os.Exit(1)
		}
	}

	// The scope of the initialization context ends here, so we call cancel to release resources associated with it.
	cancel()

	if *flHTTPBind != "" {
		ln, err := net.Listen("tcp", *flHTTPBind)
		if err != nil {
			log.Error(err, "can't bind HTTP endpoint", "endpoint", *flHTTPBind)
			os.Exit(1)
		}
		mux := http.NewServeMux()
		if *flHTTPMetrics {
			mux.Handle("/metrics", promhttp.Handler())
		}

		if *flHTTPprof {
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		}

		// This is a dumb liveliness check endpoint. Currently this checks
		// nothing and will always return 200 if the process is live.
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if !getRepoReady() {
				http.Error(w, "repo is not ready", http.StatusServiceUnavailable)
			}
			// Otherwise success
		})
		log.V(0).Info("serving HTTP", "endpoint", *flHTTPBind)
		go func() {
			err := http.Serve(ln, mux)
			log.Error(err, "HTTP server terminated")
			os.Exit(1)
		}()
	}

	// Startup webhooks goroutine
	var webhookRunner *hook.HookRunner
	if *flWebhookURL != "" {
		log := log.WithName("webhook")
		webhook := hook.NewWebhook(
			*flWebhookURL,
			*flWebhookMethod,
			*flWebhookStatusSuccess,
			*flWebhookTimeout,
			log,
		)
		webhookRunner = hook.NewHookRunner(
			webhook,
			*flWebhookBackoff,
			hook.NewHookData(),
			log,
			*flOneTime,
		)
		go webhookRunner.Run(context.Background())
	}

	// Startup exechooks goroutine
	var exechookRunner *hook.HookRunner
	if *flExechookCommand != "" {
		log := log.WithName("exechook")
		exechook := hook.NewExechook(
			cmd.NewRunner(log),
			*flExechookCommand,
			*flRoot,
			[]string{},
			*flExechookTimeout,
			log,
		)
		exechookRunner = hook.NewHookRunner(
			exechook,
			*flExechookBackoff,
			hook.NewHookData(),
			log,
			*flOneTime,
		)
		go exechookRunner.Run(context.Background())
	}

	// Setup signal notify channel
	sigChan := make(chan os.Signal, 1)
	if syncSig != 0 {
		log.V(2).Info("installing signal handler", "signal", unix.SignalName(syncSig))
		signal.Notify(sigChan, syncSig)
	}

	// Craft a function that can be called to refresh credentials when needed.
	refreshCreds := func(ctx context.Context) error {
		// These should all be mutually-exclusive configs.
		if *flUsername != "" {
			if err := git.StoreCredentials(ctx, *flUsername, *flPassword); err != nil {
				return err
			}
		}
		if *flAskPassURL != "" {
			// When using an auth URL, the credentials can be dynamic, it needs to be
			// re-fetched each time.
			if err := git.CallAskPassURL(ctx); err != nil {
				askpassCount.WithLabelValues(metricKeyError).Inc()
				return err
			}
			askpassCount.WithLabelValues(metricKeySuccess).Inc()
		}
		return nil
	}

	failCount := 0
	for {
		log.V(1).Info("syncing repo")
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), *flSyncTimeout)

		if err := git.InitRepo(ctx); err != nil {
			log.Error(err, "can't init root", "path", absRoot)
			os.Exit(1)
		}

		if changed, hash, err := git.SyncRepo(ctx, refreshCreds); err != nil {
			failCount++
			updateSyncMetrics(metricKeyError, start)
			if *flMaxFailures >= 0 && failCount > *flMaxFailures {
				// Exit after too many retries, maybe the error is not recoverable.
				log.Error(err, "too many failures, aborting", "failCount", failCount)
				os.Exit(1)
			}
			log.Error(err, "error syncing repo, will retry", "failCount", failCount)
		} else {
			// this might have been called before, but also might not have
			setRepoReady()
			if changed {
				if absTouchFile != "" {
					if err := touch(absTouchFile); err != nil {
						log.Error(err, "failed to touch-file", "path", absTouchFile)
					} else {
						log.V(4).Info("touched touch-file", "path", absTouchFile)
					}
				}
				if webhookRunner != nil {
					webhookRunner.Send(hash)
				}
				if exechookRunner != nil {
					exechookRunner.Send(hash)
				}
				updateSyncMetrics(metricKeySuccess, start)
			} else {
				updateSyncMetrics(metricKeyNoOp, start)
			}

			// Determine if git-sync should terminate for one of several reasons
			if *flOneTime {
				// Wait for hooks to complete at least once, if not nil, before
				// checking whether to stop program.
				// Assumes that if hook channels are not nil, they will have at
				// least one value before getting closed
				exitCode := 0 // is 0 if all hooks succeed, else is 1
				if exechookRunner != nil {
					if err := exechookRunner.WaitForCompletion(); err != nil {
						exitCode = 1
					}
				}
				if webhookRunner != nil {
					if err := webhookRunner.WaitForCompletion(); err != nil {
						exitCode = 1
					}
				}
				log.DeleteErrorFile()
				log.V(2).Info("exiting after one sync", "status", exitCode)
				os.Exit(exitCode)
			}

			if isHash, err := git.RevIsHash(ctx); err != nil {
				log.Error(err, "can't tell if rev is a git hash, exiting", "rev", git.rev)
				os.Exit(1)
			} else if isHash {
				log.V(0).Info("rev appears to be a git hash, no further sync needed", "rev", git.rev)
				log.DeleteErrorFile()
				sleepForever()
			}

			if failCount > 0 {
				log.V(5).Info("resetting failure count", "failCount", failCount)
				failCount = 0
			}
			log.DeleteErrorFile()
		}

		log.V(1).Info("next sync", "waitTime", flPeriod.String())
		cancel()

		// Sleep until the next sync. If syncSig is set then the sleep may
		// be interrupted by that signal.
		t := time.NewTimer(*flPeriod)
		select {
		case <-t.C:
		case <-sigChan:
			log.V(2).Info("caught signal", "signal", unix.SignalName(syncSig))
			t.Stop()
		}
	}
}

// makeAbsPath makes an absolute path from a path which might be absolute
// or relative.  If the path is already absolute, it will be used.  If it is
// not absolute, it will be joined with the provided root. If the path is
// empty, the result will be empty.
func makeAbsPath(path, root string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

// touch will try to ensure that the file at the specified path exists and that
// its timestamps are updated.
func touch(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	return file.Close()
}

const redactedString = "REDACTED"

func redactURL(urlstr string) string {
	u, err := url.Parse(urlstr)
	if err != nil {
		return err.Error()
	}
	if u.User != nil {
		u.User = url.UserPassword(u.User.Username(), redactedString)
	}
	return u.String()
}

// logSafeArgs makes sure any sensitive args (e.g. passwords) are redacted
// before logging.
func logSafeArgs(args []string) []string {
	ret := make([]string, len(args))
	redactWholeArg := false
	readactURLArg := false
	for i, arg := range args {
		if redactWholeArg {
			ret[i] = redactedString
			redactWholeArg = false
			continue
		}
		if readactURLArg {
			ret[i] = redactURL(arg)
			readactURLArg = false
			continue
		}
		// Handle --password
		if arg == "--password" {
			redactWholeArg = true
		}
		if strings.HasPrefix(arg, "--password=") {
			arg = "--password=" + redactedString
		}
		// Handle password embedded in --repo
		if arg == "--repo" {
			readactURLArg = true
		}
		if strings.HasPrefix(arg, "--repo=") {
			arg = "--repo=" + redactURL(arg[7:])
		}
		ret[i] = arg
	}
	return ret
}

// logSafeEnv makes sure any sensitive env vars (e.g. passwords) are redacted
// before logging.
func logSafeEnv(env []string) []string {
	ret := make([]string, len(env))
	for i, ev := range env {
		if strings.HasPrefix(ev, "GIT_SYNC_PASSWORD=") {
			ev = "GIT_SYNC_PASSWORD=" + redactedString
		}
		if strings.HasPrefix(ev, "GIT_SYNC_REPO=") {
			ev = "GIT_SYNC_REPO=" + redactURL(ev[14:])
		}
		ret[i] = ev
	}
	return ret
}

func normalizePath(path string) (string, error) {
	delinked, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(delinked)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// initRepo looks at the git root and initializes it if needed.  This assumes
// the root dir already exists.
func (git *repoSync) InitRepo(ctx context.Context) error {
	// Make sure the root exists.  0755 ensures that this is usable as a volume
	// when the consumer isn't running as the same UID.
	if err := os.MkdirAll(git.root, 0755); err != nil {
		return fmt.Errorf("can't make root directory: %w", err)
	}

	// Check out the git root, and see if it is already usable.
	if _, err := os.Stat(git.root); err != nil {
		return err
	}

	// Make sure the directory we found is actually usable.
	if git.SanityCheck(ctx) {
		git.log.V(0).Info("root directory is valid", "path", git.root)
		return nil
	}

	// Maybe a previous run crashed?  Git won't use this dir.
	git.log.V(0).Info("root directory exists but failed checks, cleaning up", "path", git.root)

	// We remove the contents rather than the dir itself, because a common
	// use-case is to have a volume mounted at git.root, which makes removing
	// it impossible.
	if err := removeDirContents(git.root, git.log); err != nil {
		return fmt.Errorf("can't wipe unusable root directory: %w", err)
	}

	return nil
}

// sanityCheck tries to make sure that the dir is a valid git repository.
func (git *repoSync) SanityCheck(ctx context.Context) bool {
	git.log.V(0).Info("sanity-checking git repo", "repo", git.root)

	// If it is empty, we are done.
	if empty, err := dirIsEmpty(git.root); err != nil {
		git.log.Error(err, "can't list repo directory", "repo", git.root)
		return false
	} else if empty {
		git.log.V(0).Info("git repo is empty", "repo", git.root)
		return true
	}

	// Check that this is actually the root of the repo.
	if root, err := git.run.Run(ctx, git.root, nil, git.cmd, "rev-parse", "--show-toplevel"); err != nil {
		git.log.Error(err, "can't get repo toplevel", "repo", git.root)
		return false
	} else {
		root = strings.TrimSpace(root)
		if root != git.root {
			git.log.V(0).Info("git repo is under another repo", "repo", git.root, "parent", root)
			return false
		}
	}

	// Consistency-check the repo.
	if _, err := git.run.Run(ctx, git.root, nil, git.cmd, "fsck", "--no-progress", "--connectivity-only"); err != nil {
		git.log.Error(err, "repo sanity check failed", "repo", git.root)
		return false
	}

	return true
}

func dirIsEmpty(dir string) (bool, error) {
	dirents, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(dirents) == 0, nil
}

func removeDirContents(dir string, log *logging.Logger) error {
	dirents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, fi := range dirents {
		p := filepath.Join(dir, fi.Name())
		if log != nil {
			log.V(2).Info("removing path recursively", "path", p, "isDir", fi.IsDir())
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}

	return nil
}

func updateSyncMetrics(key string, start time.Time) {
	syncDuration.WithLabelValues(key).Observe(time.Since(start).Seconds())
	syncCount.WithLabelValues(key).Inc()
}

// Do no work, but don't do something that triggers go's runtime into thinking
// it is deadlocked.
func sleepForever() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)
	<-c
	os.Exit(0)
}

// handleConfigError prints the error to the standard error, prints the usage
// if the `printUsage` flag is true, exports the error to the error file and
// exits the process with the exit code.
func handleConfigError(log *logging.Logger, printUsage bool, format string, a ...interface{}) {
	s := fmt.Sprintf(format, a...)
	fmt.Fprintln(os.Stderr, s)
	if printUsage {
		pflag.Usage()
	}
	log.ExportError(s)
	os.Exit(1)
}

// Put the current UID/GID into /etc/passwd so SSH can look it up.  This
// assumes that we have the permissions to write to it.
func addUser() error {
	// Skip if the UID already exists. The Dockerfile already adds the default UID/GID.
	if _, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil {
		return nil
	}
	home := os.Getenv("HOME")
	if home == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("can't get working directory and $HOME is not set: %w", err)
		}
		home = cwd
	}

	f, err := os.OpenFile("/etc/passwd", os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	str := fmt.Sprintf("git-sync:x:%d:%d::%s:/sbin/nologin\n", os.Getuid(), os.Getgid(), home)
	_, err = f.WriteString(str)
	return err
}

// UpdateSymlink atomically swaps the symlink to point at the specified
// directory and cleans up the previous worktree.  If there was a previous
// worktree, this returns the path to it.
func (git *repoSync) UpdateSymlink(ctx context.Context, newDir string) (string, error) {
	linkDir, linkFile := filepath.Split(git.link)

	// Make sure the link directory exists. We do this here, rather than at
	// startup because it might be under --root and that gets wiped in some
	// circumstances.
	if err := os.MkdirAll(filepath.Dir(linkDir), os.FileMode(int(0755))); err != nil {
		return "", fmt.Errorf("error making symlink dir: %v", err)
	}

	// Get currently-linked repo directory (to be removed), unless it doesn't exist
	oldWorktreePath, err := filepath.EvalSymlinks(git.link)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("error accessing current worktree: %v", err)
	}

	// newDir is absolute, so we need to change it to a relative path.  This is
	// so it can be volume-mounted at another path and the symlink still works.
	newDirRelative, err := filepath.Rel(linkDir, newDir)
	if err != nil {
		return "", fmt.Errorf("error converting to relative path: %v", err)
	}

	const tmplink = "tmp-link"
	git.log.V(1).Info("creating tmp symlink", "root", linkDir, "link", tmplink, "target", newDirRelative)
	if err := os.Symlink(newDirRelative, filepath.Join(linkDir, tmplink)); err != nil {
		return "", fmt.Errorf("error creating symlink: %v", err)
	}

	git.log.V(1).Info("renaming symlink", "root", linkDir, "oldName", tmplink, "newName", linkFile)
	if err := os.Rename(filepath.Join(linkDir, tmplink), git.link); err != nil {
		return "", fmt.Errorf("error replacing symlink: %v", err)
	}

	return oldWorktreePath, nil
}

// repoReady indicates that the repo has been cloned and synced.
var readyLock sync.Mutex
var repoReady = false

func getRepoReady() bool {
	readyLock.Lock()
	defer readyLock.Unlock()
	return repoReady
}

func setRepoReady() {
	readyLock.Lock()
	defer readyLock.Unlock()
	repoReady = true
}

// CleanupWorkTree() is used to remove a worktree and its folder
func (git *repoSync) CleanupWorkTree(ctx context.Context, gitRoot, worktree string) error {
	// Clean up worktree(s)
	git.log.V(1).Info("removing worktree", "path", worktree)
	if err := os.RemoveAll(worktree); err != nil {
		return fmt.Errorf("error removing directory: %v", err)
	} else if _, err := git.run.Run(ctx, git.root, nil, git.cmd, "worktree", "prune"); err != nil {
		return err
	}
	return nil
}

// AddWorktreeAndSwap creates a new worktree and calls UpdateSymlink to swap
// the symlink to point to the new worktree.  This returns 1) whether the link
// has changed; 2) what the new hash is; 3) was there an error to be reported.
func (git *repoSync) AddWorktreeAndSwap(ctx context.Context, hash string) (bool, string, error) {
	git.log.V(0).Info("syncing git", "rev", git.rev, "hash", hash)

	args := []string{"fetch", "-f", "--tags"}
	if git.depth != 0 {
		args = append(args, "--depth", strconv.Itoa(git.depth))
	}
	fetch := "HEAD"
	if git.branch != "" {
		fetch = git.branch
	}
	args = append(args, git.repo, fetch)

	// Update from the remote.
	if _, err := git.run.Run(ctx, git.root, nil, git.cmd, args...); err != nil {
		return false, "", err
	}

	// With shallow fetches, it's possible to race with the upstream repo and
	// end up NOT fetching the hash we wanted. If we can't resolve that hash
	// to a commit we can just end early and leave it for the next sync period.
	// This probably SHOULD be an error, but it can be a problem for startup
	// and first-sync-must-succeed semantics.  If/when we fix that this can be
	// fixed.
	if _, err := git.ResolveRef(ctx, hash); err != nil {
		git.log.Error(err, "can't resolve commit, will retry", "rev", git.rev, "hash", hash)
		return false, "", nil
	}

	// Make a worktree for this exact git hash.
	worktreePath := filepath.Join(git.root, hash)

	// Avoid wedge cases where the worktree was created but this function
	// error'd without cleaning up.  The next time thru the sync loop fails to
	// create the worktree and bails out. This manifests as:
	//     "fatal: '/repo/root/rev-nnnn' already exists"
	if err := git.CleanupWorkTree(ctx, git.root, worktreePath); err != nil {
		return false, "", err
	}

	git.log.V(0).Info("adding worktree", "path", worktreePath, "hash", hash)
	_, err := git.run.Run(ctx, git.root, nil, git.cmd, "worktree", "add", "--detach", worktreePath, hash, "--no-checkout")
	if err != nil {
		return false, "", err
	}

	// The .git file in the worktree directory holds a reference to
	// /git/.git/worktrees/<worktree-dir-name>. Replace it with a reference
	// using relative paths, so that other containers can use a different volume
	// mount name.
	worktreePathRelative, err := filepath.Rel(git.root, worktreePath)
	if err != nil {
		return false, "", err
	}
	gitDirRef := []byte(filepath.Join("gitdir: ../.git/worktrees", worktreePathRelative) + "\n")
	if err = os.WriteFile(filepath.Join(worktreePath, ".git"), gitDirRef, 0644); err != nil {
		return false, "", err
	}

	// If sparse checkout is requested, configure git for it.
	if git.sparseFile != "" {
		// This is required due to the undocumented behavior outlined here:
		// https://public-inbox.org/git/CAPig+cSP0UiEBXSCi7Ua099eOdpMk8R=JtAjPuUavRF4z0R0Vg@mail.gmail.com/t/
		git.log.V(0).Info("configuring worktree sparse checkout")
		checkoutFile := git.sparseFile

		gitInfoPath := filepath.Join(git.root, fmt.Sprintf(".git/worktrees/%s/info", hash))
		gitSparseConfigPath := filepath.Join(gitInfoPath, "sparse-checkout")

		source, err := os.Open(checkoutFile)
		if err != nil {
			return false, "", err
		}
		defer source.Close()

		if _, err := os.Stat(gitInfoPath); os.IsNotExist(err) {
			fileMode := os.FileMode(int(0755))
			err := os.Mkdir(gitInfoPath, fileMode)
			if err != nil {
				return false, "", err
			}
		}

		destination, err := os.Create(gitSparseConfigPath)
		if err != nil {
			return false, "", err
		}
		defer destination.Close()

		_, err = io.Copy(destination, source)
		if err != nil {
			return false, "", err
		}

		args := []string{"sparse-checkout", "init"}
		_, err = git.run.Run(ctx, worktreePath, nil, git.cmd, args...)
		if err != nil {
			return false, "", err
		}
	}

	// Reset the worktree's working copy to the specific rev.
	_, err = git.run.Run(ctx, worktreePath, nil, git.cmd, "reset", "--hard", hash, "--")
	if err != nil {
		return false, "", err
	}
	git.log.V(0).Info("reset worktree to hash", "path", worktreePath, "hash", hash)

	// Update submodules
	// NOTE: this works for repo with or without submodules.
	if git.submodules != submodulesOff {
		git.log.V(0).Info("updating submodules")
		submodulesArgs := []string{"submodule", "update", "--init"}
		if git.submodules == submodulesRecursive {
			submodulesArgs = append(submodulesArgs, "--recursive")
		}
		if git.depth != 0 {
			submodulesArgs = append(submodulesArgs, "--depth", strconv.Itoa(git.depth))
		}
		_, err = git.run.Run(ctx, worktreePath, nil, git.cmd, submodulesArgs...)
		if err != nil {
			return false, "", err
		}
	}

	// Change the file permissions, if requested.
	if git.chmod != 0 {
		mode := fmt.Sprintf("%#o", git.chmod)
		git.log.V(0).Info("changing file permissions", "mode", mode)
		_, err = git.run.Run(ctx, "", nil, "chmod", "-R", mode, worktreePath)
		if err != nil {
			return false, "", err
		}
	}

	// Reset the root's rev (so we can prune and so we can rely on it later).
	// Use --soft so we do not check out files into the root.
	_, err = git.run.Run(ctx, git.root, nil, git.cmd, "reset", "--soft", hash, "--")
	if err != nil {
		return false, "", err
	}
	git.log.V(0).Info("reset root to hash", "path", git.root, "hash", hash)

	// Flip the symlink.
	oldWorktree, err := git.UpdateSymlink(ctx, worktreePath)
	if err != nil {
		return false, "", err
	}
	setRepoReady()

	// From here on we have to save errors until the end.
	var cleanupErrs multiError

	// Clean up previous worktree(s).
	if oldWorktree != "" {
		if err := git.CleanupWorkTree(ctx, git.root, oldWorktree); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}

	// Run GC if needed.
	if git.gc != gcOff {
		args := []string{"gc"}
		switch git.gc {
		case gcAuto:
			args = append(args, "--auto")
		case gcAlways:
			// no extra flags
		case gcAggressive:
			args = append(args, "--aggressive")
		}
		if _, err := git.run.Run(ctx, git.root, nil, git.cmd, args...); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}

	if len(cleanupErrs) > 0 {
		return true, hash, cleanupErrs
	}
	return true, hash, nil
}

type multiError []error

func (m multiError) Error() string {
	if len(m) == 0 {
		return "<no error>"
	}
	if len(m) == 1 {
		return m[0].Error()
	}
	strs := make([]string, 0, len(m))
	for _, e := range m {
		strs = append(strs, e.Error())
	}
	return strings.Join(strs, "; ")
}

// CloneRepo does an initial clone of the git repo.
func (git *repoSync) CloneRepo(ctx context.Context) error {
	args := []string{"clone", "--no-checkout"}
	if git.branch != "" {
		args = append(args, "-b", git.branch)
	}
	if git.depth != 0 {
		args = append(args, "--depth", strconv.Itoa(git.depth))
	}
	args = append(args, git.repo, git.root)
	git.log.V(0).Info("cloning repo", "origin", git.repo, "path", git.root)

	_, err := git.run.Run(ctx, "", nil, git.cmd, args...)
	if err != nil {
		if strings.Contains(err.Error(), "already exists and is not an empty directory") {
			// Maybe a previous run crashed?  Git won't use this dir.
			git.log.V(0).Info("git root exists and is not empty (previous crash?), cleaning up", "path", git.root)
			// We remove the contents rather than the dir itself, because a
			// common use-case is to have a volume mounted at git.root, which
			// makes removing it impossible.
			err := removeDirContents(git.root, git.log)
			if err != nil {
				return err
			}
			_, err = git.run.Run(ctx, "", nil, git.cmd, args...)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	// If sparse checkout is requested, configure git for it.
	if git.sparseFile != "" {
		git.log.V(0).Info("configuring sparse checkout")
		checkoutFile := git.sparseFile

		// TODO: capture this as a function (mostly duplicated above)
		gitRepoPath := filepath.Join(git.root, ".git")
		gitInfoPath := filepath.Join(gitRepoPath, "info")
		gitSparseConfigPath := filepath.Join(gitInfoPath, "sparse-checkout")

		source, err := os.Open(checkoutFile)
		if err != nil {
			return err
		}
		defer source.Close()

		if _, err := os.Stat(gitInfoPath); os.IsNotExist(err) {
			fileMode := os.FileMode(int(0755))
			err := os.Mkdir(gitInfoPath, fileMode)
			if err != nil {
				return err
			}
		}

		destination, err := os.Create(gitSparseConfigPath)
		if err != nil {
			return err
		}
		defer destination.Close()

		_, err = io.Copy(destination, source)
		if err != nil {
			return err
		}

		args := []string{"sparse-checkout", "init"}
		_, err = git.run.Run(ctx, git.root, nil, git.cmd, args...)
		if err != nil {
			return err
		}
	}

	return nil
}

// LocalHashForRev returns the locally known hash for a given rev.
func (git *repoSync) LocalHashForRev(ctx context.Context, rev string) (string, error) {
	output, err := git.run.Run(ctx, git.root, nil, git.cmd, "rev-parse", rev)
	if err != nil {
		return "", err
	}
	return strings.Trim(string(output), "\n"), nil
}

// RemoteHashForRef returns the upstream hash for a given ref.
func (git *repoSync) RemoteHashForRef(ctx context.Context, ref string) (string, error) {
	output, err := git.run.Run(ctx, git.root, nil, git.cmd, "ls-remote", "-q", git.repo, ref)
	if err != nil {
		return "", err
	}
	parts := strings.Split(string(output), "\t")
	return parts[0], nil
}

func (git *repoSync) RevIsHash(ctx context.Context) (bool, error) {
	hash, err := git.ResolveRef(ctx, git.rev)
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(hash, git.rev), nil
}

func (git *repoSync) ResolveRef(ctx context.Context, ref string) (string, error) {
	// If git doesn't identify rev as a commit, we're done.
	output, err := git.run.Run(ctx, git.root, nil, git.cmd, "cat-file", "-t", ref)
	if err != nil {
		return "", err
	}
	o := strings.Trim(string(output), "\n")
	if o != "commit" {
		return "", nil
	}

	// `git cat-file -t` also returns "commit" for tags. If rev is already a git
	// hash, the output will be the same hash as the input.  Of course, a user
	// could specify "abc" and match "abcdef12345678", so we just do a prefix
	// match.
	output, err = git.LocalHashForRev(ctx, ref)
	if err != nil {
		return "", err
	}
	return output, nil
}

// SyncRepo syncs the branch of a given repository to the link at the given rev.
// returns (1) whether a change occured, (2) the new hash, and (3) an error if one happened
func (git *repoSync) SyncRepo(ctx context.Context, refreshCreds func(context.Context) error) (bool, string, error) {
	refreshCreds(ctx)

	currentWorktreeGit := filepath.Join(git.link, ".git")
	var hash string
	_, err := os.Stat(currentWorktreeGit)
	switch {
	case os.IsNotExist(err):
		// First time. Just clone it and get the hash.
		err = git.CloneRepo(ctx)
		if err != nil {
			return false, "", err
		}
		hash, err = git.ensureLocalHashForRev(ctx, git.rev)
		if err != nil {
			return false, "", err
		}
	case err != nil:
		return false, "", fmt.Errorf("error checking if a worktree exists %q: %v", currentWorktreeGit, err)
	default:
		// Not the first time. Figure out if the ref has changed.
		local, remote, err := git.GetRevs(ctx)
		if err != nil {
			return false, "", err
		}
		if local == remote {
			git.log.V(1).Info("no update required", "rev", git.rev, "local", local, "remote", remote)
			return false, "", nil
		}
		git.log.V(0).Info("update required", "rev", git.rev, "local", local, "remote", remote)
		hash = remote
	}

	return git.AddWorktreeAndSwap(ctx, hash)
}

func (git *repoSync) ensureLocalHashForRev(ctx context.Context, rev string) (string, error) {
	hash, err := git.LocalHashForRev(ctx, rev)
	if err == nil {
		return hash, nil
	}

	// git might return either error, based on flags used internal to
	// LocalHashForRev, so we will consider either one to be a "rev not found".
	if es := err.Error(); !stringContainsOneOf(es, "unknown revision", "bad revision") {
		return "", err
	}

	// The rev was not found, try to fetch it.
	git.log.V(1).Info("rev was not found, trying fetch", "rev", git.rev)
	args := []string{"fetch", "-f", "--tags"}
	if git.depth != 0 {
		args = append(args, "--depth", strconv.Itoa(git.depth))
	}
	args = append(args, git.repo, "--")
	if _, err := git.run.Run(ctx, git.root, nil, git.cmd, args...); err != nil {
		return "", err
	}

	// Try again.
	hash, err = git.LocalHashForRev(ctx, git.rev)
	if err != nil {
		return "", err
	}
	return hash, nil
}

func stringContainsOneOf(s string, matches ...string) bool {
	for _, m := range matches {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// GetRevs returns the local and upstream hashes for rev.
func (git *repoSync) GetRevs(ctx context.Context) (string, string, error) {
	// Ask git what the exact hash is for rev.
	local, err := git.LocalHashForRev(ctx, git.rev)
	if err != nil {
		return "", "", err
	}

	// Build a ref string, depending on whether the user asked to track HEAD or
	// a tag.
	ref := "HEAD"
	if git.rev != "HEAD" {
		ref = "refs/tags/" + git.rev
	} else if git.branch != "" {
		ref = "refs/heads/" + git.branch
	}

	// Figure out what hash the remote resolves ref to.
	remote, err := git.RemoteHashForRef(ctx, ref)
	if err != nil {
		return "", "", err
	}

	return local, remote, nil
}

func md5sum(s string) string {
	h := md5.New()
	io.WriteString(h, s)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// StoreCredentials stores the username and password for later use.
func (git *repoSync) StoreCredentials(ctx context.Context, username, password string) error {
	git.log.V(3).Info("storing git credentials")
	git.log.V(9).Info("md5 of credentials", "username", md5sum(username), "password", md5sum(password))

	creds := fmt.Sprintf("url=%v\nusername=%v\npassword=%v\n", git.repo, username, password)
	_, err := git.run.RunWithStdin(ctx, "", nil, creds, git.cmd, "credential", "approve")
	if err != nil {
		return fmt.Errorf("can't configure git credentials: %w", err)
	}

	return nil
}

func (git *repoSync) SetupGitSSH(setupKnownHosts bool, pathToSSHSecret, pathToSSHKnownHosts string) error {
	git.log.V(1).Info("setting up git SSH credentials")

	// If the user sets GIT_SSH_COMMAND we try to respect it.
	sshCmd := os.Getenv("GIT_SSH_COMMAND")
	if sshCmd == "" {
		sshCmd = "ssh"
	}

	if _, err := os.Stat(pathToSSHSecret); err != nil {
		return fmt.Errorf("can't access SSH key file %s: %w", pathToSSHSecret, err)
	}
	sshCmd += fmt.Sprintf(" -i %s", pathToSSHSecret)

	if setupKnownHosts {
		if _, err := os.Stat(pathToSSHKnownHosts); err != nil {
			return fmt.Errorf("can't access SSH known_hosts file %s: %w", pathToSSHKnownHosts, err)
		}
		sshCmd += fmt.Sprintf(" -o StrictHostKeyChecking=yes -o UserKnownHostsFile=%s", pathToSSHKnownHosts)
	} else {
		sshCmd += fmt.Sprintf(" -o StrictHostKeyChecking=no")
	}

	git.log.V(9).Info("setting GIT_SSH_COMMAND", "value", sshCmd)
	if err := os.Setenv("GIT_SSH_COMMAND", sshCmd); err != nil {
		return fmt.Errorf("can't set $GIT_SSH_COMMAND: %w", err)
	}

	return nil
}

func (git *repoSync) SetupCookieFile(ctx context.Context) error {
	git.log.V(1).Info("configuring git cookie file")

	var pathToCookieFile = "/etc/git-secret/cookie_file"

	_, err := os.Stat(pathToCookieFile)
	if err != nil {
		return fmt.Errorf("can't access git cookiefile: %w", err)
	}

	if _, err = git.run.Run(ctx, "", nil, git.cmd, "config", "--global", "http.cookiefile", pathToCookieFile); err != nil {
		return fmt.Errorf("can't configure git cookiefile: %w", err)
	}

	return nil
}

// CallAskPassURL consults the specified URL looking for git credentials in the
// response.
//
// The expected URL callback output is below,
// see https://git-scm.com/docs/gitcredentials for more examples:
//
//	username=xxx@example.com
//	password=xxxyyyzzz
func (git *repoSync) CallAskPassURL(ctx context.Context) error {
	git.log.V(2).Info("calling auth URL to get credentials")

	var netClient = &http.Client{
		Timeout: time.Second * 1,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	httpReq, err := http.NewRequestWithContext(ctx, "GET", git.authURL, nil)
	if err != nil {
		return fmt.Errorf("can't create auth request: %w", err)
	}
	resp, err := netClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("can't access auth URL: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != 200 {
		errMessage, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("auth URL returned status %d, failed to read body: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("auth URL returned status %d, body: %q", resp.StatusCode, string(errMessage))
	}
	authData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("can't read auth response: %w", err)
	}

	username := ""
	password := ""
	for _, line := range strings.Split(string(authData), "\n") {
		keyValues := strings.SplitN(line, "=", 2)
		if len(keyValues) != 2 {
			continue
		}
		switch keyValues[0] {
		case "username":
			username = keyValues[1]
		case "password":
			password = keyValues[1]
		}
	}

	if err := git.StoreCredentials(ctx, username, password); err != nil {
		return err
	}

	return nil
}

func (git *repoSync) setupDefaultGitConfigs(ctx context.Context) error {
	configs := []keyVal{{
		// Never auto-detach GC runs.
		key: "gc.autoDetach",
		val: "false",
	}, {
		// Fairly aggressive GC.
		key: "gc.pruneExpire",
		val: "now",
	}, {
		// How to manage credentials (for those modes that need it).
		key: "credential.helper",
		val: "cache --timeout 3600",
	}}
	for _, kv := range configs {
		if _, err := git.run.Run(ctx, "", nil, git.cmd, "config", "--global", kv.key, kv.val); err != nil {
			return fmt.Errorf("error configuring git %q %q: %v", kv.key, kv.val, err)
		}
	}
	return nil
}

func (git *repoSync) setupExtraGitConfigs(ctx context.Context, configsFlag string) error {
	git.log.V(1).Info("setting additional git configs")

	configs, err := parseGitConfigs(configsFlag)
	if err != nil {
		return fmt.Errorf("can't parse --git-config flag: %v", err)
	}
	for _, kv := range configs {
		if _, err := git.run.Run(ctx, "", nil, git.cmd, "config", "--global", kv.key, kv.val); err != nil {
			return fmt.Errorf("error configuring additional git configs %q %q: %v", kv.key, kv.val, err)
		}
	}

	return nil
}

type keyVal struct {
	key string
	val string
}

func parseGitConfigs(configsFlag string) ([]keyVal, error) {
	ch := make(chan rune)
	stop := make(chan bool)
	go func() {
		for _, r := range configsFlag {
			select {
			case <-stop:
				break
			default:
				ch <- r
			}
		}
		close(ch)
		return
	}()

	result := []keyVal{}

	// This assumes it is at the start of a key.
	for {
		cur := keyVal{}
		var err error

		// Peek and see if we have a key.
		if r, ok := <-ch; !ok {
			break
		} else {
			// This can accumulate things that git doesn't allow, but we'll
			// just let git handle it, rather than try to pre-validate to their
			// spec.
			if r == '"' {
				cur.key, err = parseGitConfigQKey(ch)
				if err != nil {
					return nil, err
				}
			} else {
				cur.key, err = parseGitConfigKey(r, ch)
				if err != nil {
					return nil, err
				}
			}
		}

		// Peek and see if we have a value.
		if r, ok := <-ch; !ok {
			return nil, fmt.Errorf("key %q: no value", cur.key)
		} else {
			if r == '"' {
				cur.val, err = parseGitConfigQVal(ch)
				if err != nil {
					return nil, fmt.Errorf("key %q: %v", cur.key, err)
				}
			} else {
				cur.val, err = parseGitConfigVal(r, ch)
				if err != nil {
					return nil, fmt.Errorf("key %q: %v", cur.key, err)
				}
			}
		}

		result = append(result, cur)
	}

	return result, nil
}

func parseGitConfigQKey(ch <-chan rune) (string, error) {
	str, err := parseQString(ch)
	if err != nil {
		return "", err
	}

	// The next character must be a colon.
	r, ok := <-ch
	if !ok {
		return "", fmt.Errorf("unexpected end of key: %q", str)
	}
	if r != ':' {
		return "", fmt.Errorf("unexpected character after quoted key: %q%c", str, r)
	}
	return str, nil
}

func parseGitConfigKey(r rune, ch <-chan rune) (string, error) {
	buf := make([]rune, 0, 64)
	buf = append(buf, r)

	for r := range ch {
		switch {
		case r == ':':
			return string(buf), nil
		default:
			buf = append(buf, r)
		}
	}
	return "", fmt.Errorf("unexpected end of key: %q", string(buf))
}

func parseGitConfigQVal(ch <-chan rune) (string, error) {
	str, err := parseQString(ch)
	if err != nil {
		return "", err
	}

	// If there is a next character, it must be a comma.
	r, ok := <-ch
	if ok && r != ',' {
		return "", fmt.Errorf("unexpected character after quoted value %q%c", str, r)
	}
	return str, nil
}

func parseGitConfigVal(r rune, ch <-chan rune) (string, error) {
	buf := make([]rune, 0, 64)
	buf = append(buf, r)

	for r := range ch {
		switch r {
		case '\\':
			if r, err := unescape(ch); err != nil {
				return "", err
			} else {
				buf = append(buf, r)
			}
		case ',':
			return string(buf), nil
		default:
			buf = append(buf, r)
		}
	}
	// We ran out of characters, but that's OK.
	return string(buf), nil
}

func parseQString(ch <-chan rune) (string, error) {
	buf := make([]rune, 0, 64)

	for r := range ch {
		switch r {
		case '\\':
			if e, err := unescape(ch); err != nil {
				return "", err
			} else {
				buf = append(buf, e)
			}
		case '"':
			return string(buf), nil
		default:
			buf = append(buf, r)
		}
	}
	return "", fmt.Errorf("unexpected end of quoted string: %q", string(buf))
}

// unescape processes most of the documented escapes that git config supports.
func unescape(ch <-chan rune) (rune, error) {
	r, ok := <-ch
	if !ok {
		return 0, fmt.Errorf("unexpected end of escape sequence")
	}
	switch r {
	case 'n':
		return '\n', nil
	case 't':
		return '\t', nil
	case '"', ',', '\\':
		return r, nil
	}
	return 0, fmt.Errorf("unsupported escape character: '%c'", r)
}

// This string is formatted for 80 columns.  Please keep it that way.
// DO NOT USE TABS.
var manual = `
GIT-SYNC

NAME
    git-sync - sync a remote git repository

SYNOPSIS
    git-sync --repo=<repo> [OPTION]...

DESCRIPTION

    Fetch a remote git repository to a local directory, poll the remote for
    changes, and update the local copy.

    This is a perfect "sidecar" container in Kubernetes.  For example, it can
    periodically pull files down from a repository so that an application can
    consume them.

    git-sync can pull one time, or on a regular interval.  It can read from the
    HEAD of a branch, from a git tag, or from a specific git hash.  It will only
    re-pull if the target has changed in the remote repository.  When it
    re-pulls, it updates the destination directory atomically.  In order to do
    this, it uses a git worktree in a subdirectory of the --root and flips a
    symlink.

    git-sync can pull over HTTP(S) (with authentication or not) or SSH.

    git-sync can also be configured to make a webhook call upon successful git
    repo synchronization.  The call is made after the symlink is updated.

OPTIONS

    Many options can be specified as either a commandline flag or an environment
    variable.

    --add-user, $GIT_SYNC_ADD_USER
            Add a record to /etc/passwd for the current UID/GID.  This is
            needed to use SSH with an arbitrary UID (see --ssh).  This assumes
            that /etc/passwd is writable by the current UID.

    --askpass-url <string>, $GIT_SYNC_ASKPASS_URL
            A URL to query for git credentials.  The query must return success
            (200) and produce a series of key=value lines, including
            "username=<value>" and "password=<value>".

    --branch <string>, $GIT_SYNC_BRANCH
            The git branch to check out.  If not specified, this defaults to
            the default branch of --repo.

    --change-permissions <int>, $GIT_SYNC_PERMISSIONS
            Change permissions on the checked-out files to the specified mode.

    --cookie-file <string>, $GIT_SYNC_COOKIE_FILE
            Use a git cookiefile (/etc/git-secret/cookie_file) for
            authentication.

    --depth <int>, $GIT_SYNC_DEPTH
            Create a shallow clone with history truncated to the specified
            number of commits.  If not specified, this defaults to cloning the
            full history of the repo.

    --error-file <string>, $GIT_SYNC_ERROR_FILE
            The path to an optional file into which errors will be written.
            This may be an absolute path or a relative path, in which case it
            is relative to --root.  If it is relative to --root, the first path
            element may not start with a period.

    --exechook-backoff <duration>, $GIT_SYNC_EXECHOOK_BACKOFF
            The time to wait before retrying a failed --exechook-command.  If
            not specified, this defaults to 3 seconds ("3s").

    --exechook-command <string>, $GIT_SYNC_EXECHOOK_COMMAND
            An optional command to be executed after syncing a new hash of the
            remote repository.  This command does not take any arguments and
            executes with the synced repo as its working directory.  The
            environment variable $GITSYNC_HASH will be set to the git SHA that
            was synced.  The execution is subject to the overall --sync-timeout
            flag and will extend the effective period between sync attempts.
            This flag obsoletes --sync-hook-command, but if sync-hook-command
            is specified, it will take precedence.

    --exechook-timeout <duration>, $GIT_SYNC_EXECHOOK_TIMEOUT
            The timeout for the --exechook-command.  If not specifid, this
            defaults to 30 seconds ("30s").

    --git <string>, $GIT_SYNC_GIT
            The git command to run (subject to PATH search, mostly for
            testing).  This defaults to "git".

    --git-config <string>, $GIT_SYNC_GIT_CONFIG
            Additional git config options in a comma-separated 'key:val'
            format.  The parsed keys and values are passed to 'git config' and
            must be valid syntax for that command.

            Both keys and values can be either quoted or unquoted strings.
            Within quoted keys and all values (quoted or not), the following
            escape sequences are supported:
                '\n' => [newline]
                '\t' => [tab]
                '\"' => '"'
                '\,' => ','
                '\\' => '\'
            To include a colon within a key (e.g. a URL) the key must be
            quoted.  Within unquoted values commas must be escaped.  Within
            quoted values commas may be escaped, but are not required to be.
            Any other escape sequence is an error.

    --git-gc <string>, $GIT_SYNC_GIT_GC
            The git garbage collection behavior: one of "auto", "always",
            "aggressive", or "off".  If not specified, this defaults to
            "auto".

            - auto: Run "git gc --auto" once per successful sync.  This mode
              respects git's gc.* config params.
            - always: Run "git gc" once per successful sync.
            - aggressive: Run "git gc --aggressive" once per successful sync.
              This mode can be slow and may require a longer --sync-timeout value.
            - off: Disable explicit git garbage collection, which may be a good
              fit when also using --one-time.

    -h, --help
            Print help text and exit.

    --http-bind <string>, $GIT_SYNC_HTTP_BIND
            The bind address (including port) for git-sync's HTTP endpoint.  If
            not specified, the HTTP endpoint is not enabled.

    --http-metrics, $GIT_SYNC_HTTP_METRICS
            Enable metrics on git-sync's HTTP endpoint, if it is enabled (see
            --http-bind).

    --http-pprof, $GIT_SYNC_HTTP_PPROF
            Enable the pprof debug endpoints on git-sync's HTTP endpoint, if it
            is enabled (see --http-bind).

    --link <string>, $GIT_SYNC_LINK
            The path to at which to create a symlink which points to the
            current git directory, at the currently synced SHA.  This may be an
            absolute path or a relative path, in which case it is relative to
            --root.  The last path element is the name of the link and must not
            start with a period.  Consumers of the synced files should always
            use this link - it is updated atomically and should always be
            valid.  The basename of the target of the link is the current SHA.
            If not specified, this defaults to the leaf dir of --repo.

    --man
            Print this manual and exit.

    --max-failures <int>, $GIT_SYNC_MAX_FAILURES
            The number of consecutive failures allowed before aborting (the
            first sync must succeed), Setting this to a negative value will
            retry forever after the initial sync.  If not specified, this
            defaults to 0, meaning any sync failure will terminate git-sync.

    --one-time, $GIT_SYNC_ONE_TIME
            Exit after one sync.

    --password <string>, $GIT_SYNC_PASSWORD
            The password or personal access token (see github docs) to use for
            git authentication (see --username).  NOTE: for security reasons,
            users should prefer --password-file or $GIT_SYNC_PASSWORD_FILE for
            specifying the password.

    --password-file <string>, $GIT_SYNC_PASSWORD_FILE
            The file from which the password or personal access token (see
            github docs) to use for git authentication (see --username) will be
            read.

    --period <duration>, $GIT_SYNC_PERIOD
            How long to wait between sync attempts.  This must be at least
            10ms.  This flag obsoletes --wait, but if --wait is specified, it
            will take precedence.  If not specified, this defaults to 10
            seconds ("10s").

    --repo <string>, $GIT_SYNC_REPO
            The git repository to sync.  This flag is required.

    --rev <string>, $GIT_SYNC_REV
            The git revision (tag or hash) to check out.  If not specified,
            this defaults to "HEAD".

    --root <string>, $GIT_SYNC_ROOT
            The root directory for git-sync operations, under which --link will
            be created.  This must be a path that either a) does not exist (it
            will be created); b) is an empty directory; or c) is a directory
            which can be emptied by removing all of the contents.  This flag is
            required.

    --sparse-checkout-file <string>, $GIT_SYNC_SPARSE_CHECKOUT_FILE
            The path to a git sparse-checkout file (see git documentation for
            details) which controls which files and directories will be checked
            out.  If not specified, the default is to check out the entire repo.

    --ssh, $GIT_SYNC_SSH
            Use SSH for git authentication and operations.

    --ssh-key-file <string>, $GIT_SYNC_SSH_KEY_FILE
            The SSH key to use when using --ssh.  If not specified, this
            defaults to "/etc/git-secret/ssh".

    --ssh-known-hosts, $GIT_SYNC_KNOWN_HOSTS
            Enable SSH known_hosts verification when using --ssh.  If not
            specified, this defaults to true.

    --ssh-known-hosts-file <string>, $GIT_SYNC_SSH_KNOWN_HOSTS_FILE
            The known_hosts file to use when --ssh-known-hosts is specified.
            If not specified, this defaults to "/etc/git-secret/known_hosts".

    --submodules <string>, $GIT_SYNC_SUBMODULES
            The git submodule behavior: one of "recursive", "shallow", or
            "off".  If not specified, this defaults to "recursive".

    --sync-on-signal <string>, $GIT_SYNC_SYNC_ON_SIGNAL
            Indicates that a sync attempt should occur upon receipt of the 
            specified signal name (e.g. SIGHUP) or number (e.g. 1). If a sync
            is already in progress, another sync will be triggered as soon as
            the current one completes. If not specified, signals will not 
            trigger syncs.

    --sync-timeout <duration>, $GIT_SYNC_SYNC_TIMEOUT
            The total time allowed for one complete sync.  This must be at least
            10ms.  This flag obsoletes --timeout, but if --timeout is specified,
            it will take precedence.  If not specified, this defaults to 120
            seconds ("120s").

    --touch-file <string>, $GIT_SYNC_TOUCH_FILE
            The path to an optional file which will be touched whenever a sync
            completes.  This may be an absolute path or a relative path, in
            which case it is relative to --root.  If it is relative to --root,
            the first path element may not start with a period.

    --username <string>, $GIT_SYNC_USERNAME
            The username to use for git authentication (see --password-file or
            --password).

    -v, --verbose <int>
            Set the log verbosity level.  Logs at this level and lower will be
            printed.

    --version
            Print the version and exit.

    --webhook-backoff <duration>, $GIT_SYNC_WEBHOOK_BACKOFF
            The time to wait before retrying a failed --webhook-url.  If not
            specified, this defaults to 3 seconds ("3s").

    --webhook-method <string>, $GIT_SYNC_WEBHOOK_METHOD
            The HTTP method for the --webhook-url.  If not specified, this defaults to "POST".

    --webhook-success-status <int>, $GIT_SYNC_WEBHOOK_SUCCESS_STATUS
            The HTTP status code indicating a successful --webhook-url.  Setting
            this to -1 disables success checks to make webhooks
            "fire-and-forget".  If not specified, this defaults to 200.

    --webhook-timeout <duration>, $GIT_SYNC_WEBHOOK_TIMEOUT
            The timeout for the --webhook-url.  If not specified, this defaults
            to 1 second ("1s").

    --webhook-url <string>, $GIT_SYNC_WEBHOOK_URL
            A URL for optional webhook notifications when syncs complete.  The
            header 'Gitsync-Hash' will be set to the git SHA that was synced.

EXAMPLE USAGE

    git-sync \
        --repo=https://github.com/kubernetes/git-sync \
        --branch=main \
        --rev=HEAD \
        --period=10s \
        --root=/mnt/git

AUTHENTICATION

    Git-sync offers several authentication options to choose from.  If none of
    the following are specified, git-sync will try to access the repo in the
    "natural" manner.  For example, "https://repo" will try to use plain HTTPS
    and "git@example.com:repo" will try to use SSH.

    username/password
            The --username (GIT_SYNC_USERNAME) and --password-file
            (GIT_SYNC_PASSWORD_FILE) or --password (GIT_SYNC_PASSWORD) flags
            will be used.  To prevent password leaks, the --password-file flag
            or GIT_SYNC_PASSWORD environment variable is almost always
            preferred to the --password flag.

            A variant of this is --askpass-url (GIT_SYNC_ASKPASS_URL), which
            consults a URL (e.g. http://metadata) to get credentials on each
            sync.

    SSH
            When --ssh (GIT_SYNC_SSH) is specified, the --ssh-key-file
            (GIT_SYNC_SSH_KEY_FILE) will be used.  Users are strongly advised
            to also use --ssh-known-hosts (GIT_SYNC_KNOWN_HOSTS) and
            --ssh-known-hosts-file (GIT_SYNC_SSH_KNOWN_HOSTS_FILE) when using
            SSH.

    cookies
            When --cookie-file (GIT_SYNC_COOKIE_FILE) is specified, the
            associated cookies can contain authentication information.

HOOKS

    Webhooks and exechooks are executed asynchronously from the main git-sync
    process.  If a --webhook-url or --exechook-command is configured, whenever
    a new hash is synced the hook(s) will be invoked.  For exechook, that means
    the command is exec()'ed, and for webhooks that means an HTTP request is
    sent using the method defined in --webhook-method.  Git-sync will retry
    both forms of hooks until they succeed (exit code 0 for exechooks, or
    --webhook-success-status for webhooks).  If unsuccessful, git-sync will
    wait --exechook-backoff or --webhook-backoff (as appropriate) before
    re-trying the hook.

    Hooks are not guaranteed to succeed on every single SHA change.  For example,
    if a hook fails and a new SHA is synced during the backoff period, the
    retried hook will fire for the newest SHA.
`

func printManPage() {
	fmt.Print(manual)
}
