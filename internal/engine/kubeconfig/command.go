package kubeconfig

import (
	"context"
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// canonicalDir is config.CanonicalDir behind a package var, so a test can
// make the directory impossible to make absolute (a removed working
// directory on Linux) without changing the process's own.
var canonicalDir = config.CanonicalDir

type kubeconfigCmd struct {
	dir     string
	store   config.CredStore
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewKubeconfig builds the top-level `kubeconfig` command (local): it emits an
// exec-credential kubeconfig on stdout, delegating token creation to `auth
// get-token` rather than embedding a long-lived credential.
func NewCommand(dir string, store config.CredStore, clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &kubeconfigCmd{dir: dir, store: store, clients: clients, roots: roots}
}

func (kubeconfigCmd) Describe() runtime.Descriptor {
	cluster := engine.ClusterFlag(true, false)
	cluster.Desc = "cluster `id|name` to generate the kubeconfig for"
	return runtime.Descriptor{
		Name:  "kubeconfig",
		Usage: "Print an exec-credential kubeconfig for a cluster, with its direct endpoint contexts when the cluster has them",
		Desc: "Prints a kubeconfig whose user entry runs the rancher exec-credential plugin. One context reaches the cluster " +
			"through the Rancher proxy. When the cluster has the Authorized Cluster Endpoint enabled and the multi-cluster-management " +
			"feature is on, the file also carries the direct contexts Rancher's own kubeconfig carries: <entry>-fqdn when the endpoint " +
			"names an fqdn, else one <entry>-<node> per control-plane node, and the direct context is current. When those contexts " +
			"cannot be derived (the endpoint's feature is off, the nodes cannot be read, no control-plane node has an address), the " +
			"proxy-only file is printed and one notice on stderr says why. The user entry's env records this config directory, absolute, " +
			"and RANCHER_CLI_CREDENTIAL_STORE (empty when the directory declares its store, else the value set now), so the file works " +
			"for this user on this machine only; notices on stderr say so, and name the stored login kubectl's exec plugin will use when " +
			"it is not this session's. The exec entry names that login's recorded owner with --user-id, so the plugin hands kubectl only " +
			"that user's tokens; a notice says when the login records no owner. installHint says how to install rancher, or how to " +
			"regenerate the file for another --exec-command.",
		Flags: []runtime.ArgSpec{
			cluster,
			{Name: "exec-command", Type: "string", Desc: "`command` kubectl runs for the exec plugin: a name looked up on PATH where kubectl runs, or a path, " +
				"made absolute from the working directory (a leading ~ is the home directory); default rancher"},
		},
		Examples: []string{
			"rancher kubeconfig --cluster prod > prod.yaml",
			"rancher kubeconfig --cluster prod --exec-command ~/bin/rancher > prod.yaml",
		},
		CommandType:  runtime.Local,
		ServerAccess: runtime.ServerAccessRead,
		Auth:         true,
		// RawBytes is a bare kubeconfig YAML document - it never varies with
		// -o, so every format but the default table would silently accept
		// and then ignore it (the create kubeconfig precedent).
		OutputPolicy: runtime.OutputPolicy{AllowedFormats: []string{"table"}},
		Shape:        runtime.ShapeBytes,
		// The file it writes names this directory, and its remedies are
		// read where that directory may not be the shell's.
		RemedyConfigDir: true,
	}
}

func (c *kubeconfigCmd) Execute(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	cfg, err := config.Load(config.FilePath(c.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	// Effective (not CurrentServerEntry) so an env-only session (RANCHER_CLI_URL/
	// RANCHER_CLI_TOKEN, no stored server) works exactly like every other
	// command.
	eff, err := cfg.Effective(c.store)
	if err != nil {
		return runtime.Result{}, err
	}
	if eff.URL == "" {
		// Config-aware: after a logout the server entries survive; name
		// the re-login shortcut instead of implying a blank machine.
		return runtime.Result{}, cfg.ErrNotLoggedIn()
	}

	// Defense-in-depth: the pipeline's validateInput normally rejects a
	// missing --cluster first, but Execute must be safe when called directly.
	id := in.String("cluster")
	if id == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--cluster is required")
	}
	execValue := in.String("exec-command")
	command, commandIsPath, err := execCommand(execValue, execValue != "" || in.IsSet("exec-command"))
	if err != nil {
		return runtime.Result{}, err
	}
	// The exec entry names the directory absolute, so kubectl's working
	// directory and environment never choose another one; it is the form
	// a cache record carries.
	dir := canonicalDir(c.dir)
	if dir == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, fmt.Sprintf("the config directory %s cannot be made absolute", clierr.ShellQuote(c.dir)))
	}
	id, err = c.clients.ResolveCluster(ctx, id)
	if err != nil {
		return runtime.Result{}, err
	}

	// Effective already cleared eff.CA/eff.ServerName if RANCHER_CLI_URL diverged
	// from the stored server - every command sharing this choke point, not
	// just kubeconfig, so there is nothing kubeconfig-specific left to check
	// here.
	caPEM, err := config.ResolveCA(eff.CA)
	if err != nil {
		return runtime.Result{}, err
	}

	base := kube.BaseURL(eff.URL)
	if !strings.HasPrefix(strings.ToLower(base), "https://") {
		// client-go silently refuses to wire exec-credential auth for a non-https
		// server (no error, no Authorization header) - warn so the resulting
		// kubeconfig's silent 401s are explainable. Inverted (warn on
		// NOT-https, case-insensitive) rather than matching "http://" literally:
		// BaseURL's error-fallback path can return a malformed or differently
		// cased scheme that a literal "http://" prefix check would miss.
		p.Notice("server URL is not https; kubectl will silently skip exec-credential auth - use https")
	}
	// eff.ServerName is empty for an env-only session, or when the env
	// override just moved the URL away from the stored server - synthesize a
	// stable entry name from the server host instead.
	serverName := eff.ServerName
	if serverName == "" {
		serverName = config.ServerHost(base)
	}
	entryName := fmt.Sprintf("%s-%s", serverName, id)
	if err := reservedName(entryName, id, base, eff.CA, eff.CASource); err != nil {
		return runtime.Result{}, err
	}

	// The plugin's own lookup, run once: it decides the --user-id the
	// exec entry carries and the notices below.
	lc := lookupCall{cfg: cfg, dir: dir, server: base, serverID: config.CanonicalServerIdentity(base),
		name: entryName, clusterID: id, caSource: eff.CA, eff: eff, envToken: os.Getenv(config.EnvToken) != "", store: c.store}
	lk := lc.lookup()
	owner, ownerNotice := lc.recordedOwner(lk)

	cluster := &clientcmdapi.Cluster{Server: base + "/k8s/clusters/" + id}
	execArgs := []string{"auth", "get-token", "--server", base, "--cluster", id, "--name", entryName}
	if owner != "" {
		execArgs = append(execArgs, "--user-id", owner)
	}
	if caPEM != "" {
		cluster.CertificateAuthorityData = []byte(caPEM)
		// The exec plugin resolves its own CA from the current server at
		// invocation time, which may not be this server if the user has since
		// switched servers. Pin the CA baked into this kubeconfig explicitly so
		// the cached exec plugin can never authenticate against the wrong trust
		// root.
		execArgs = append(execArgs, "--cacert", caPEM)
	}

	// The direct endpoint contexts Rancher's own kubeconfig carries, read
	// from the same management objects. An error here is a cancelled
	// context, a cluster that vanished after resolution or a refused
	// credential (401); every other failure is a reason in Omitted and the
	// file stays proxy-only.
	mgmt, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	direct, err := DirectEndpoints(ctx, mgmt, steve.RootFactoryGetter(c.roots), id)
	if err != nil {
		return runtime.Result{}, err
	}

	store := recordedStore(cfg)
	kc := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{entryName: cluster},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{entryName: {
			Exec: &clientcmdapi.ExecConfig{
				APIVersion:      "client.authentication.k8s.io/v1beta1",
				Command:         command,
				Args:            execArgs,
				Env:             execEnv(dir, store),
				InstallHint:     installHint(command, eff.ServerName, id, dir, base, caPEM != ""),
				InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode,
				// With a direct context in the file the plugin must learn
				// which server kubectl is about to use, so it can wait for
				// a fresh token to be accepted there; kubectl passes the
				// invoking context's cluster entry in KUBERNETES_EXEC_INFO.
				ProvideClusterInfo: len(direct.Endpoints) > 0,
			},
		}},
		Contexts: map[string]*clientcmdapi.Context{entryName: {
			Cluster:  entryName,
			AuthInfo: entryName,
		}},
		CurrentContext: entryName,
	}
	// Every direct context shares the one exec user entry, as Rancher's own
	// file shares one token across them. The fqdn context, else the first
	// Ready node, becomes current; with no Ready node the proxy stays.
	for _, ep := range direct.Endpoints {
		name := entryName + "-" + ep.Name
		kc.Clusters[name] = &clientcmdapi.Cluster{Server: ep.Server, CertificateAuthorityData: ep.CAData}
		kc.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: entryName}
		if ep.Ready && kc.CurrentContext == entryName {
			kc.CurrentContext = name
		}
	}
	if len(direct.Omitted) > 0 && p != nil {
		reasons := make([]string, len(direct.Omitted))
		for i, r := range direct.Omitted {
			// A reason can relay a remedy (a timed-out read names
			// rancher inspect cluster), which the prompter expands.
			reasons[i] = runtime.PrintableRemedies(r)
		}
		p.Notice("direct endpoint contexts omitted: " + strings.Join(reasons, "; "))
	}
	if p != nil {
		if n := lookupNotice(lc, lk); n != "" {
			p.Notice(n)
		}
		if ownerNotice != "" {
			p.Notice(ownerNotice)
		}
		p.Notice(machineNotice(dir, store, command, commandIsPath))
	}

	b, err := clientcmd.Write(kc)
	if err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{RawBytes: b}, nil
}
