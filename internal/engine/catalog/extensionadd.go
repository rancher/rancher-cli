package catalog

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	catalogcomplete "github.com/rancher/rancher-cli/internal/engine/catalog/complete"
	"github.com/rancher/rancher-cli/internal/engine/catalog/extension"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// extensionRepositoryHelp names both repositories with the labels of the
// Rancher UI's checkboxes, for the command's description.
const extensionRepositoryHelp = "partner-extensions (the Rancher UI's \"Add Partners Extensions Repository\"), offered on every Rancher server, and " +
	"rancher-ui-plugins (the UI's \"Add Rancher Prime Extensions Repository\"), offered only on Rancher Prime"

type addRepositoryCmd struct {
	clients engine.ClusterClientFactory
}

// NewExtensionAddRepository builds `extension add-repository <name>`
// (mutating): the ClusterRepo of one of the two Rancher extension
// repositories, with the three fields the Rancher UI's Extensions page sends.
func NewExtensionAddRepository(clients engine.ClusterClientFactory) runtime.Command {
	return &addRepositoryCmd{clients: clients}
}

func (*addRepositoryCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "extension add-repository",
		Usage: "Add a Rancher extension repository",
		Desc: "Adds one of the two repositories of Rancher UI extensions, as the Rancher UI's Extensions page does: " +
			extensionRepositoryHelp + ". " +
			"It creates a ClusterRepo on the local cluster with the repository's name, git URL and branch main, and nothing else. " +
			"A ClusterRepo that already reads the same URL (its status.url, spec.gitRepo or spec.url, the first that is set) is refused with CONFLICT. " +
			"Rancher downloads the repository from github.com after the command; a server with no access to github.com cannot use it. " +
			"The command reads the server's edition for rancher-ui-plugins; on a server that is not Rancher Prime, " +
			"or whose edition cannot be read, that repository is refused with VALIDATION.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{{
			Name: "name", Type: "string", Required: true,
			Desc: "the repository to add: `partner-extensions`, or `rancher-ui-plugins` on Rancher Prime",
		}},
		Examples: []string{
			"rancher extension add-repository partner-extensions --dry-run",
			"rancher extension add-repository partner-extensions --apply",
		},
		Destructive: "false",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

func (c *addRepositoryCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <name> is required")
	}
	name := args[0]
	repo, ok := extension.RepositoryNamed(name)
	if !ok {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, clierr.IDText(name)+
			" is not a Rancher extension repository; the Rancher extension repositories are partner-extensions and, on Rancher Prime, rancher-ui-plugins")
	}
	if repo.PrimeOnly {
		if err := requirePrime(ctx, c.clients, repo.Name); err != nil {
			return safety.Effect{}, nil, err
		}
	}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	k := kind("clusterrepo")
	list, err := client.List(ctx, k.GVR, "", "")
	if err != nil {
		return safety.Effect{}, nil, err
	}
	existing := make([]extension.ClusterRepo, 0, len(list.Items))
	for _, obj := range list.Items {
		existing = append(existing, clusterRepoOf(&obj))
	}
	if found, added := extension.AlreadyAdded(existing, repo.URL); added {
		return safety.Effect{}, nil, clierr.New(clierr.CodeConflict,
			"the repository "+clierr.IDText(repo.URL)+" is already added as ClusterRepo "+clierr.ShellQuote(found.Name)).
			WithHintCommand(clierr.NewCommand("get clusterrepo").Arg(found.Name).FlagValue("--cluster", engine.LocalCluster))
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.cattle.io/v1",
		"kind":       "ClusterRepo",
		"metadata":   map[string]any{"name": repo.Name},
		"spec":       map[string]any{"gitRepo": repo.URL, "gitBranch": repo.Branch},
	}}
	eff, exec, err := engine.PlanCreate(ctx, client, k.GVR, "", obj, k.Redaction(), engine.LocalCluster, k.Name, "ClusterRepo", false)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	return eff, func(ctx context.Context) (runtime.Result, error) {
		res, err := exec(ctx)
		if err != nil {
			return res, err
		}
		res.Warnings = append(res.Warnings, downloadNote(repo.Name))
		return res, nil
	}, nil
}

// requirePrime refuses a Prime-only repository on a server that is not Rancher
// Prime or whose edition cannot be read. It asks for the edition only here.
func requirePrime(ctx context.Context, clients engine.ClusterClientFactory, name string) error {
	edition := engine.EditionUnknown
	if provider, ok := clients.(engine.ServerEditionProvider); ok {
		if got, err := provider.ServerEdition(ctx); err == nil {
			edition = got
		}
	}
	switch edition {
	case engine.EditionPrime:
		return nil
	case engine.EditionCommunity:
		return clierr.New(clierr.CodeValidation, name+
			" is offered only on Rancher Prime and this server is not Rancher Prime; the repository every edition offers is partner-extensions")
	}
	return clierr.New(clierr.CodeValidation,
		"the edition of this server could not be read; "+name+" is offered only on Rancher Prime and is not added; the repository every edition offers is partner-extensions")
}

// downloadNote is the warning after a successful add: the ClusterRepo only
// names the repository, and Rancher downloads it afterwards.
func downloadNote(name string) string {
	return clierr.Text("Rancher downloads the repository from github.com after this command",
		clierr.Lead("; read its state with: "),
		clierr.NewCommand("get clusterrepo").Arg(name).FlagValue("--cluster", engine.LocalCluster),
		"; a server with no access to github.com (an air-gapped server) cannot download it")
}

// clusterRepoOf reads the fields of a ClusterRepo that tell where it reads
// charts from.
func clusterRepoOf(obj *unstructured.Unstructured) extension.ClusterRepo {
	statusURL, _, _ := unstructured.NestedString(obj.Object, "status", "url")
	gitRepo, _, _ := unstructured.NestedString(obj.Object, "spec", "gitRepo")
	url, _, _ := unstructured.NestedString(obj.Object, "spec", "url")
	return extension.ClusterRepo{Name: obj.GetName(), StatusURL: statusURL, GitRepo: gitRepo, URL: url}
}

// LiveSource hands the surface the source of the repository name.
func (c *addRepositoryCmd) LiveSource() completion.Source {
	return catalogcomplete.AddRepositorySource(c.Describe().Name)
}
