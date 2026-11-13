package catalog

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusedValues are values a command cannot print so that every supported
// shell reads them as written.
var refusedValues = map[string]string{
	"apostrophe":         "o'b",
	"right single quote": "o\u2019b",
	"zero width space":   "o\u200bb",
	"two backslashes":    `o\\b`,
}

// targetWith is the target of the tests with one field set to v.
func targetWith(field, v string) actionTarget {
	t := actionTarget{typedCluster: testTypedCluster, namespace: testNS, release: testRelease}
	switch field {
	case "cluster":
		t.typedCluster = v
	case "namespace":
		t.namespace = v
	case "release":
		t.release = v
	}
	return t
}

var targetFields = []string{"cluster", "namespace", "release"}

// requireLeftOut asserts text holds no command and the note once.
func requireLeftOut(t *testing.T, text string) {
	t.Helper()
	assert.NotContains(t, text, clierr.Rancher, "a command is printed")
	assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote), "the note appears once: %q", text)
}

func resultData(t *testing.T, res runtime.Result) result {
	t.Helper()
	data, ok := res.Data.(result)
	require.True(t, ok)
	return data
}

// TestActionResultLeavesOutOnlyTheFollowUpWithARefusedOperationName asserts a
// refused operation name drops the get operation follow-up from the list and
// the text, keeps the get app follow-up and writes the note once.
func TestActionResultLeavesOutOnlyTheFollowUpWithARefusedOperationName(t *testing.T) {
	t.Parallel()
	getApp := clierr.Rancher + " get app 'my-nginx' --cluster 'prod' --namespace 'web'"
	for name, v := range refusedValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := actionResult("install", targetWith("", ""), actionAnswer{OperationName: v, OperationNamespace: testNS}, true)
			assert.Equal(t, []string{getApp}, resultData(t, res).FollowUp)
			assert.Contains(t, res.Text, "follow it with: "+getApp+"\n")
			assert.NotContains(t, res.Text, "get operation")
			assert.Equal(t, 1, strings.Count(res.Text, clierr.LeftOutNote))
		})
	}
}

// TestActionResultLeavesOutEveryFollowUpWithARefusedCluster asserts a refused
// cluster leaves both follow-ups out, with an empty list and the note once.
func TestActionResultLeavesOutEveryFollowUpWithARefusedCluster(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := actionResult("uninstall", targetWith("cluster", v), actionAnswer{OperationName: testOperation, OperationNamespace: testNS}, true)
			assert.Equal(t, []string{}, resultData(t, res).FollowUp)
			requireLeftOut(t, res.Text)
			assert.NotContains(t, res.Text, "follow it with")
		})
	}
}

// TestActionResultLeavesOutTheGetAppFollowUpWithARefusedNamespaceOrRelease
// asserts a refused namespace or release leaves the get app follow-up out and
// keeps the get operation one, which names the operation's own namespace.
func TestActionResultLeavesOutTheGetAppFollowUpWithARefusedNamespaceOrRelease(t *testing.T) {
	t.Parallel()
	getOperation := clierr.Rancher + " get operation 'helm-operation-abcde' --cluster 'prod' --namespace 'web' --wait"
	for name, v := range refusedValues {
		for _, field := range []string{"namespace", "release"} {
			t.Run(name+" "+field, func(t *testing.T) {
				t.Parallel()
				res := actionResult("uninstall", targetWith(field, v), actionAnswer{OperationName: testOperation, OperationNamespace: testNS}, true)
				assert.Equal(t, []string{getOperation}, resultData(t, res).FollowUp)
				assert.Contains(t, res.Text, "follow it with: "+getOperation+"\n")
				assert.NotContains(t, res.Text, "get app")
				assert.Equal(t, 1, strings.Count(res.Text, clierr.LeftOutNote))
			})
		}
	}
}

// TestWaitedResultLeavesOutTheGetAppFollowUp asserts a refused target leaves
// the get app follow-up out of a waited result: an empty list, the note once.
func TestWaitedResultLeavesOutTheGetAppFollowUp(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range targetFields {
			t.Run(name+" "+field, func(t *testing.T) {
				t.Parallel()
				res := waitedResult("uninstall", targetWith(field, v), actionAnswer{OperationName: testOperation, OperationNamespace: testNS}, waitEnd{})
				assert.Equal(t, []string{}, resultData(t, res).FollowUp)
				requireLeftOut(t, res.Text)
				assert.NotContains(t, res.Text, "follow it with")
			})
		}
	}
}

// TestWaitedResultWithAGoneAppHasNoNote asserts an App that is gone leaves the
// follow-up list empty and writes no note.
func TestWaitedResultWithAGoneAppHasNoNote(t *testing.T) {
	t.Parallel()
	res := waitedResult("uninstall", targetWith("", ""), actionAnswer{}, waitEnd{gone: true})
	assert.Equal(t, []string{}, resultData(t, res).FollowUp)
	assert.NotContains(t, res.Text, clierr.LeftOutNote)
}

// waitErrorUses reports whether the command of the wait error builder holds
// the target field: every builder names the get app command, which holds all
// three, except these two.
func waitErrorUses(builder, field string) bool {
	switch builder {
	case "noRevisionError":
		return field == "cluster"
	case "uninstalledButFailedError":
		return field != "release"
	}
	return true
}

// TestWaitErrorsLeaveOutTheirCommandsForARefusedTarget asserts every error of
// the wait whose command holds a refused value prints no command: no hint, no
// recovery (the text details hold no recover line), the note once in the
// message. An error that passes a hint through keeps it.
func TestWaitErrorsLeaveOutTheirCommandsForARefusedTarget(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range targetFields {
			for builder, build := range waitErrorBuilders {
				if !waitErrorUses(builder, field) {
					continue
				}
				t.Run(name+" "+field+" "+builder, func(t *testing.T) {
					t.Parallel()
					w := &appWait{t: targetWith(field, v), op: actionAnswer{OperationName: testOperation, OperationNamespace: testNS}, filter: Values{}.filterFunc()}
					ce, ok := errors.AsType[*clierr.Error](build(w))
					require.True(t, ok)
					d, ok := ce.Details.(clierr.CommittedContextDetails)
					require.True(t, ok)
					if builder == "passThroughError" {
						assert.Equal(t, testGetOperationsHint, ce.Hint)
						assert.Equal(t, testGetOperationsHint, d.Recovery)
						return
					}
					assert.Empty(t, ce.Hint)
					assert.Empty(t, d.Recovery)
					for _, line := range d.TextDetails() {
						assert.NotContains(t, line.String(), "recover:", "a left-out command leaves no recover line")
					}
					requireLeftOut(t, ce.Message)
				})
			}
		}
	}
}

// TestWaitPassThroughErrorWithoutAHintLeavesOutTheGetApp asserts an error with
// no hint gains no get app hint for a refused target and says so.
func TestWaitPassThroughErrorWithoutAHintLeavesOutTheGetApp(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := &appWait{t: targetWith("cluster", v), filter: Values{}.filterFunc()}
			ce, ok := errors.AsType[*clierr.Error](w.passThroughError(errors.New("connection reset")))
			require.True(t, ok)
			assert.Empty(t, ce.Hint)
			requireLeftOut(t, ce.Message)
		})
	}
}

// TestNoRevisionErrorLeavesOutTheReadOperationCommand asserts a refused
// operation name or namespace leaves the get operation command out of the
// text and the hint; the facts stay.
func TestNoRevisionErrorLeavesOutTheReadOperationCommand(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for field, op := range map[string]actionAnswer{
			"operation name":      {OperationName: v, OperationNamespace: testNS},
			"operation namespace": {OperationName: testOperation, OperationNamespace: v},
		} {
			t.Run(name+" "+field, func(t *testing.T) {
				t.Parallel()
				w := &appWait{t: targetWith("", ""), op: op, filter: Values{}.filterFunc()}
				rev := &revision{version: 2, status: "failed"}
				ce := w.noRevisionError(stall{}, rev)
				assert.Empty(t, ce.Hint)
				assert.Equal(t, clierr.CommittedContextDetails{Committed: true, ResourceKind: "app", ResourceName: testRelease}, ce.Details)
				requireLeftOut(t, ce.Message)
				assert.NotContains(t, ce.Message, "read the operation with")
				assert.True(t, strings.HasSuffix(ce.Message, "release 'my-nginx' is at revision "+strconv.FormatInt(rev.version, 10)+", status failed; "+clierr.LeftOutNote), ce.Message)
			})
		}
	}
}

// TestUninstallPreviewTextsLeaveOutThePreviewCommand asserts the errors that
// send the reader to the uninstall preview drop the command and the words
// that introduce it for a refused target; the facts stay.
func TestUninstallPreviewTextsLeaveOutThePreviewCommand(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range targetFields {
			for builder, build := range map[string]func(w *appWait) *clierr.Error{
				"failureError":     func(w *appWait) *clierr.Error { return w.failureError(revision{status: "failed"}, &stall{}) },
				"leftPendingError": func(w *appWait) *clierr.Error { return w.leftPendingError(nil) },
				"cutOffError":      func(w *appWait) *clierr.Error { return w.cutOffError() },
			} {
				t.Run(name+" "+field+" "+builder, func(t *testing.T) {
					t.Parallel()
					w := &appWait{t: targetWith(field, v), op: actionAnswer{OperationName: testOperation, OperationNamespace: testNS}, filter: Values{}.filterFunc()}
					ce := build(w)
					requireLeftOut(t, ce.Message)
					assert.NotContains(t, ce.Message, "add --apply --yes")
					assert.NotContains(t, ce.Message, "preview of")
				})
			}
		}
	}
}

// TestInstallOverAFailedAppLeavesOutThePreviewCommand asserts the conflict
// for a failed App of the release name keeps its facts and drops the uninstall
// preview and the hint for a refused value.
func TestInstallOverAFailedAppLeavesOutThePreviewCommand(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range targetFields {
			t.Run(name+" "+field, func(t *testing.T) {
				t.Parallel()
				tgt := targetWith(field, v)
				f := newAppFake(t)
				f.put("apps", tgt.namespace, tgt.release, map[string]any{"spec": map[string]any{"info": map[string]any{"status": "failed"}}})
				err := installPlan{client: f, target: tgt}.checkNoApp(t.Context())
				ce := clierr.From(err)
				assert.Equal(t, clierr.CodeConflict, ce.Code)
				assert.Empty(t, ce.Hint)
				assert.Equal(t, "app "+clierr.ShellQuote(tgt.release)+" already exists in namespace "+clierr.ShellQuote(tgt.namespace)+" (status failed); "+clierr.LeftOutNote, ce.Message)
			})
		}
	}
}

// TestInstallOverADeployedAppLeavesOutTheGetAppHint asserts a refused value
// leaves the get app hint out of the conflict for an App that is not failed.
func TestInstallOverADeployedAppLeavesOutTheGetAppHint(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tgt := targetWith("cluster", v)
			f := newAppFake(t)
			f.put("apps", tgt.namespace, tgt.release, map[string]any{"spec": map[string]any{"info": map[string]any{"status": "deployed"}}})
			ce := clierr.From(installPlan{client: f, target: tgt}.checkNoApp(t.Context()))
			assert.Empty(t, ce.Hint)
			assert.Equal(t, "app 'my-nginx' already exists in namespace 'web' (status deployed); "+clierr.LeftOutNote, ce.Message)
		})
	}
}

// TestMissingNamespaceLeavesOutTheCreateCommands asserts a refused namespace
// or cluster leaves the create namespace command out of the text and the
// hint; the facts stay.
func TestMissingNamespaceLeavesOutTheCreateCommands(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range []string{"cluster", "namespace"} {
			t.Run(name+" "+field, func(t *testing.T) {
				t.Parallel()
				tgt := targetWith(field, v)
				if field == "cluster" {
					tgt.namespace = "absent"
				}
				f := newAppFake(t)
				ce := clierr.From(installPlan{client: f, target: tgt}.checkNamespace(t.Context()))
				assert.Equal(t, clierr.CodeNotFound, ce.Code)
				assert.Empty(t, ce.Hint)
				assert.Equal(t, "namespace "+clierr.ShellQuote(tgt.namespace)+" not found on cluster "+clierr.ShellQuote(tgt.typedCluster)+
					"; create it first; a project member creates it in the project (<project> is its name); "+clierr.LeftOutNote, ce.Message)
			})
		}
	}
}

// TestRunningOperationRefusalLeavesOutTheGetOperationHint asserts a refused
// operation name, cluster or namespace leaves the hint out of the refusal.
func TestRunningOperationRefusalLeavesOutTheGetOperationHint(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range []string{"operation", "cluster", "namespace"} {
			t.Run(name+" "+field, func(t *testing.T) {
				t.Parallel()
				tgt, op := targetWith(field, v), testOperation
				if field == "operation" {
					op = v
				}
				ce := clierr.From(operationRunning(tgt, op))
				assert.Equal(t, clierr.CodeConflict, ce.Code)
				assert.Empty(t, ce.Hint)
				requireLeftOut(t, ce.Message)
				assert.True(t, strings.HasSuffix(ce.Message, "Rancher runs a new one for each request; "+clierr.LeftOutNote), ce.Message)
			})
		}
	}
}

// TestUnknownOutcomeLeavesOutTheCommandsForARefusedTarget asserts a request
// whose outcome is unknown, and an answer that names no operation, keep their
// facts and drop the get app command and the hint for a refused target.
func TestUnknownOutcomeLeavesOutTheCommandsForARefusedTarget(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		for _, field := range []string{"cluster", "namespace"} {
			t.Run(name+" "+field, func(t *testing.T) {
				t.Parallel()
				tgt := targetWith(field, v)
				tgt.requestID = testRequestID
				unknown := clierr.From(outcomeUnknown(tgt, errors.New("connection reset"), Values{}))
				assert.Empty(t, unknown.Hint)
				requireLeftOut(t, unknown.Message)
				assert.Contains(t, unknown.Message, "this request's id is "+testRequestID)

				f := newAppFake(t)
				f.postAnswer = func(string) ([]byte, error) { return []byte(`{}`), nil }
				_, err := postAction(t.Context(), f, "/x", map[string]any{}, tgt, Values{})
				noOperation := clierr.From(err)
				assert.Empty(t, noOperation.Hint)
				requireLeftOut(t, noOperation.Message)
			})
		}
	}
}

// TestRepoRemediesLeaveOutTheListCommandForARefusedValue asserts a refused
// cluster leaves the get clusterrepos hint out, and a refused repository name
// leaves the get clusterrepo hint of a disabled one out; the message says so.
func TestRepoRemediesLeaveOutTheListCommandForARefusedValue(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			remedy := clierr.From(withRepoRemedy(clierr.New(clierr.CodeNotFound, "gone"), v))
			assert.Empty(t, remedy.Hint)
			assert.Equal(t, "gone; "+clierr.LeftOutNote, remedy.Message)

			f := newAppFake(t)
			_, err := readClusterRepo(t.Context(), f, "absent", v)
			missing := clierr.From(err)
			assert.Empty(t, missing.Hint)
			assert.Equal(t, "ClusterRepo 'absent' not found; "+clierr.LeftOutNote, missing.Message)

			f.put("clusterrepos", "", v, map[string]any{"spec": map[string]any{"enabled": false}})
			_, err = readClusterRepo(t.Context(), f, v, testTypedCluster)
			disabled := clierr.From(err)
			assert.Empty(t, disabled.Hint)
			assert.Equal(t, "ClusterRepo "+clierr.ShellQuote(v)+" is disabled; "+clierr.LeftOutNote, disabled.Message)
		})
	}
}

// TestChartsHintIsLeftOutForARefusedValue asserts the command that lists a
// repository's charts is left out for a refused cluster or repository name.
func TestChartsHintIsLeftOutForARefusedValue(t *testing.T) {
	t.Parallel()
	ok := installPlan{target: targetWith("", ""), repo: testRepo}.chartsHint("--all-versions")
	assert.True(t, ok.OK())
	assert.Equal(t, clierr.Rancher+" charts --cluster 'prod' --repo 'repo1' --all-versions", ok.Field())
	for name, v := range refusedValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.False(t, installPlan{target: targetWith("cluster", v), repo: testRepo}.chartsHint().OK())
			assert.False(t, installPlan{target: targetWith("", ""), repo: v}.chartsHint("--all-versions").OK())
		})
	}
}

// TestChartNamespaceAnnotationLeavesOutTheFlagClause asserts a chart whose
// annotation names a namespace that cannot be printed keeps the refusal and
// drops the clause that asks for --namespace with it.
func TestChartNamespaceAnnotationLeavesOutTheFlagClause(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := installPlan{target: targetWith("", ""), chart: testChart, version: "1.0.0"}
			entry := ChartVersion{Version: "1.0.0", Annotations: map[string]string{annotationNamespace: v}}
			_, err := p.checkChart(t.Context(), entry, Info{}, nil, nil, false)
			ce := clierr.From(err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, "chart 'nginx' '1.0.0' installs only in namespace "+clierr.ShellQuote(v)+" (annotation "+annotationNamespace+"); "+clierr.LeftOutNote, ce.Message)
			assert.NotContains(t, ce.Message, "run it with")
		})
	}
}

// TestChartNamespaceAnnotationKeepsTheFlagClause asserts the clause that asks
// for --namespace stays for a namespace that can be printed.
func TestChartNamespaceAnnotationKeepsTheFlagClause(t *testing.T) {
	t.Parallel()
	p := installPlan{target: targetWith("", ""), chart: testChart, version: "1.0.0"}
	entry := ChartVersion{Version: "1.0.0", Annotations: map[string]string{annotationNamespace: "cattle-system"}}
	_, err := p.checkChart(t.Context(), entry, Info{}, nil, nil, false)
	require.ErrorContains(t, err, "installs only in namespace 'cattle-system' (annotation "+annotationNamespace+"); run it with --namespace 'cattle-system'")
}

// TestUninstallLeavesOutTheListCommandsForARefusedCluster asserts a refused
// cluster leaves the get apps hint of a missing App out, and the get apps
// command of the warning about a CRD chart, which stays.
func TestUninstallLeavesOutTheListCommandsForARefusedCluster(t *testing.T) {
	t.Parallel()
	for name, v := range refusedValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := uninstallInput(func(i *appInput) { i.s["cluster"] = v })

			f := newAppFake(t)
			_, _, err := newTestUninstall(f).Plan(t.Context(), in)
			ce := clierr.From(err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Empty(t, ce.Hint)
			assert.Equal(t, "app 'my-nginx' not found in namespace 'web'; "+clierr.LeftOutNote, ce.Message)

			f = newAppFake(t)
			deployedApp(f, "deployed", map[string]any{annotationAutoInstall: "nginx-crd=match"})
			eff, _ := plan(t, newTestUninstall(f), in)
			assert.Contains(t, eff.Warnings, "the chart installed nginx-crd with it, which stays installed; "+clierr.LeftOutNote)
		})
	}
}
