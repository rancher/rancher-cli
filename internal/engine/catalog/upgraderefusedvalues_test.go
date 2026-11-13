package catalog

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
)

// upgradeRefusalPlan is the plan of an upgrade of the test release, with one
// value that cannot be printed in the field.
func upgradeRefusalPlan(field, v string) upgradePlan {
	p := upgradePlan{
		target:  targetWith("", ""),
		repo:    testRepo,
		chart:   testChart,
		version: "1.0.0",
		current: "1.0.0",
		app:     appRecord{name: testRelease, chart: testChart, version: "1.0.0"},
	}
	switch field {
	case "cluster", "namespace", "release":
		p.target = targetWith(field, v)
	case "repo":
		p.repo = v
	case "chart":
		p.chart = v
		p.app.chart = v
	}
	return p
}

// TestUpgradeStatusRefusalsLeaveOutTheirCommandsForARefusedValue asserts the
// refusals of an App Helm cannot upgrade drop each command that holds a value
// that cannot be printed, with its lead-in words, keep the facts and write the
// note once. The uninstalled refusal also names the install command, which
// holds the chart and the repository.
func TestUpgradeStatusRefusalsLeaveOutTheirCommandsForARefusedValue(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range []string{"cluster", "namespace", "release"} {
			for _, status := range []string{"uninstalled", "uninstalling", "pending-upgrade"} {
				t.Run(name+" "+field+" "+status, func(t *testing.T) {
					t.Parallel()
					p := upgradeRefusalPlan(field, v)
					p.app.status = status
					ce := clierr.From(refuseStatus(p.target, p.app))
					assert.Equal(t, clierr.CodeConflict, ce.Code)
					assert.Empty(t, ce.Hint)
					assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote), ce.Message)
					assert.NotContains(t, ce.Message, "read it with")
					assert.NotContains(t, ce.Message, "preview of")
					if status == "uninstalled" && field == "release" {
						assert.Contains(t, ce.Message, " with "+clierr.Rancher+" app install ")
						return
					}
					assert.NotContains(t, ce.Message, clierr.Rancher)
				})
			}
		}
	}
}

// TestUpgradeUninstalledRefusalLeavesOutTheInstallCommandForARefusedChartOrRepo
// asserts a chart or a source repository that cannot be printed leaves the
// install command out of the uninstalled refusal and keeps the get app hint.
func TestUpgradeUninstalledRefusalLeavesOutTheInstallCommandForARefusedChartOrRepo(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range []string{"chart", "repo"} {
			t.Run(name+" "+field, func(t *testing.T) {
				t.Parallel()
				p := upgradeRefusalPlan("", "")
				p.app.status = "uninstalled"
				p.app.annotations = map[string]string{annotationSourceRepoType: sourceRepoTypeCluster, annotationSourceRepo: testRepo}
				if field == "chart" {
					p.app.chart = v
				} else {
					p.app.annotations[annotationSourceRepo] = v
				}
				ce := clierr.From(refuseStatus(p.target, p.app))
				assert.Equal(t, clierr.CodeConflict, ce.Code)
				assert.Equal(t, clierr.Rancher+" get app 'my-nginx' --cluster 'prod' --namespace 'web'", ce.Hint)
				assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote), ce.Message)
				assert.NotContains(t, ce.Message, "app install")
				assert.True(t, strings.HasPrefix(ce.Message, "release 'my-nginx' is uninstalled and Helm keeps its history; "), ce.Message)
			})
		}
	}
}

// TestUpgradeOtherChartRefusalLeavesOutTheInstallCommand asserts the refusal
// of a chart other than the App's drops the install command, and the words
// that introduce it, when the cluster, namespace, repository or the chart
// asked for cannot be printed.
func TestUpgradeOtherChartRefusalLeavesOutTheInstallCommand(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range []string{"cluster", "namespace", "repo", "wanted"} {
			t.Run(name+" "+field, func(t *testing.T) {
				t.Parallel()
				want := "other"
				if field == "wanted" {
					want = v
				}
				p := upgradeRefusalPlan(field, v)
				ce := clierr.From(p.otherChart(want))
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Empty(t, ce.Hint)
				requireLeftOut(t, ce.Message)
				assert.NotContains(t, ce.Message, ", run ")
				assert.True(t, strings.HasSuffix(ce.Message, "as a new release; "+clierr.LeftOutNote), ce.Message)
			})
		}
	}
}

// TestUpgradeChartAnnotationRefusalsLeaveOutTheInstallCommand asserts the
// refusals of a target version that installs only in another namespace or
// under another release name keep the facts and the way out, and drop the
// install command for a value that cannot be printed.
func TestUpgradeChartAnnotationRefusalsLeaveOutTheInstallCommand(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range []string{"cluster", "repo", "chart"} {
			for _, annotation := range []string{annotationNamespace, annotationReleaseName} {
				t.Run(name+" "+field+" "+annotation, func(t *testing.T) {
					t.Parallel()
					p := upgradeRefusalPlan(field, v)
					entry := ChartVersion{Version: "1.0.0", Annotations: map[string]string{annotation: "elsewhere"}}
					_, err := p.checkChart(t.Context(), entry, Info{}, nil, nil, false)
					ce := clierr.From(err)
					assert.Equal(t, clierr.CodeValidation, ce.Code)
					requireLeftOut(t, ce.Message)
					assert.Contains(t, ce.Message, "stay on the installed version '1.0.0', or install chart ")
					assert.NotContains(t, ce.Message, " with ")
				})
			}
		}
	}
}

// TestUpgradeNamespaceAnnotationWithARefusedNamespaceLeavesOutTheInstallCommand
// asserts a namespace the annotation names that cannot be printed leaves out
// the install command, which would carry it.
func TestUpgradeNamespaceAnnotationWithARefusedNamespaceLeavesOutTheInstallCommand(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := upgradeRefusalPlan("", "")
			entry := ChartVersion{Version: "1.0.0", Annotations: map[string]string{annotationNamespace: v}}
			_, err := p.checkChart(t.Context(), entry, Info{}, nil, nil, false)
			ce := clierr.From(err)
			requireLeftOut(t, ce.Message)
			assert.Contains(t, ce.Message, "stay on the installed version '1.0.0', or install chart ")
			assert.Contains(t, ce.Message, " as a new release in "+clierr.ShellQuote(v))
		})
	}
}

// TestUpgradeInfoErrorsLeaveOutTheListCommandForARefusedValue asserts the
// refusals of a version, a chart or a repository that Rancher no longer has
// keep their facts and drop the charts hint for a cluster or repository name
// that cannot be printed.
func TestUpgradeInfoErrorsLeaveOutTheListCommandForARefusedValue(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range []string{"cluster", "repo"} {
			for _, text := range []string{helmNoVersionText, helmNoVersionsText, helmNoChartText} {
				t.Run(name+" "+field+" "+text, func(t *testing.T) {
					t.Parallel()
					p := upgradeRefusalPlan(field, v)
					body := []byte(apiserverError(text))
					err := clierr.FromResponse("GET /x on the Rancher server", http.MethodGet, http.StatusInternalServerError, body, nil)
					ce := clierr.From(p.infoError(err))
					assert.Equal(t, clierr.CodeValidation, ce.Code)
					assert.Empty(t, ce.Hint)
					assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote), ce.Message)
					assert.NotContains(t, ce.Message, clierr.Rancher)
				})
			}
		}
	}
}

// TestUpgradeFailureErrorLeavesOutTheCommandsForARefusedTarget asserts the
// error of a failed upgrade drops the upgrade and uninstall previews, and the
// words that introduce them, for a target that cannot be printed; the facts
// stay.
func TestUpgradeFailureErrorLeavesOutTheCommandsForARefusedTarget(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range targetFields {
			t.Run(name+" "+field, func(t *testing.T) {
				t.Parallel()
				w := &appWait{t: targetWith(field, v), filter: Values{}.filterFunc()}
				failed := revision{version: 3, status: "failed"}
				ce := w.upgradeFailureError(failed, &stall{})
				assert.Empty(t, ce.Hint)
				requireLeftOut(t, ce.Message)
				assert.NotContains(t, ce.Message, "preview of")
				assert.NotContains(t, ce.Message, "add --apply --yes")
				assert.Contains(t, ce.Message, "Helm does not roll back")
				assert.Contains(t, ce.Message, fmt.Sprintf("the next upgrade sends the values of revision %d, the revision that failed", failed.version))
			})
		}
	}
}

// TestUpgradeFailureErrorNamesBothPreviews asserts the error of a failed
// upgrade names the upgrade preview and the uninstall preview, each as one
// command.
func TestUpgradeFailureErrorNamesBothPreviews(t *testing.T) {
	t.Parallel()
	w := &appWait{t: targetWith("", ""), filter: Values{}.filterFunc()}
	failed := revision{version: 3, status: "failed"}
	ce := w.upgradeFailureError(failed, &stall{})
	assert.Contains(t, ce.Message, fmt.Sprintf("; to upgrade again with a change, add the change to this preview command, read the preview, then add --apply --yes: %s app upgrade 'my-nginx' --cluster 'prod' --namespace 'web' --dry-run; "+
		"the next upgrade sends the values of revision %d, the revision that failed; ", clierr.Rancher, failed.version))
	assert.Contains(t, ce.Message, "; to remove the release, add --apply --yes after reading the preview of "+
		clierr.Rancher+" app uninstall 'my-nginx' --cluster 'prod' --namespace 'web' --dry-run")
	assert.NotContains(t, ce.Message, clierr.LeftOutNote)
}
