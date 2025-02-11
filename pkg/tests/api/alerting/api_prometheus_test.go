package alerting

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"testing"
	"time"

	"github.com/grafana/grafana/pkg/models"
	apimodels "github.com/grafana/grafana/pkg/services/ngalert/api/tooling/definitions"
	ngmodels "github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/tests/testinfra"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrometheusRules(t *testing.T) {
	dir, path := testinfra.CreateGrafDir(t, testinfra.GrafanaOpts{
		EnableFeatureToggles: []string{"ngalert"},
		AnonymousUserRole:    models.ROLE_EDITOR,
	})
	store := testinfra.SetUpDatabase(t, dir)
	grafanaListedAddr := testinfra.StartGrafana(t, dir, path, store)

	// Create the namespace we'll save our alerts to.
	require.NoError(t, createFolder(t, store, 0, "default"))

	interval, err := model.ParseDuration("10s")
	require.NoError(t, err)

	// When we have no alerting rules, it returns an empty list.
	{
		promRulesURL := fmt.Sprintf("http://%s/api/prometheus/grafana/api/v1/rules", grafanaListedAddr)
		// nolint:gosec
		resp, err := http.Get(promRulesURL)
		require.NoError(t, err)
		t.Cleanup(func() {
			err := resp.Body.Close()
			require.NoError(t, err)
		})
		b, err := ioutil.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, 200, resp.StatusCode)
		require.JSONEq(t, `{"status": "success", "data": {"groups": []}}`, string(b))
	}

	// Now, let's create some rules
	{
		rules := apimodels.PostableRuleGroupConfig{
			Name: "arulegroup",
			Rules: []apimodels.PostableExtendedRuleNode{
				{
					ApiRuleNode: &apimodels.ApiRuleNode{
						For:         interval,
						Labels:      map[string]string{"label1": "val1"},
						Annotations: map[string]string{"annotation1": "val1"},
					},
					// this rule does not explicitly set no data and error states
					// therefore it should get the default values
					GrafanaManagedAlert: &apimodels.PostableGrafanaRule{
						Title:     "AlwaysFiring",
						Condition: "A",
						Data: []ngmodels.AlertQuery{
							{
								RefID: "A",
								RelativeTimeRange: ngmodels.RelativeTimeRange{
									From: ngmodels.Duration(time.Duration(5) * time.Hour),
									To:   ngmodels.Duration(time.Duration(3) * time.Hour),
								},
								Model: json.RawMessage(`{
									"datasourceUid": "-100",
									"type": "math",
									"expression": "2 + 3 > 1"
									}`),
							},
						},
					},
				},
				{
					GrafanaManagedAlert: &apimodels.PostableGrafanaRule{
						Title:     "AlwaysFiringButSilenced",
						Condition: "A",
						Data: []ngmodels.AlertQuery{
							{
								RefID: "A",
								RelativeTimeRange: ngmodels.RelativeTimeRange{
									From: ngmodels.Duration(time.Duration(5) * time.Hour),
									To:   ngmodels.Duration(time.Duration(3) * time.Hour),
								},
								Model: json.RawMessage(`{
									"datasourceUid": "-100",
									"type": "math",
									"expression": "2 + 3 > 1"
									}`),
							},
						},
						NoDataState:  apimodels.NoDataState(ngmodels.Alerting),
						ExecErrState: apimodels.ExecutionErrorState(ngmodels.KeepLastStateErrState),
					},
				},
			},
		}
		buf := bytes.Buffer{}
		enc := json.NewEncoder(&buf)
		err := enc.Encode(&rules)
		require.NoError(t, err)

		u := fmt.Sprintf("http://%s/api/ruler/grafana/api/v1/rules/default", grafanaListedAddr)
		// nolint:gosec
		resp, err := http.Post(u, "application/json", &buf)
		require.NoError(t, err)
		t.Cleanup(func() {
			err := resp.Body.Close()
			require.NoError(t, err)
		})
		b, err := ioutil.ReadAll(resp.Body)
		require.NoError(t, err)

		assert.Equal(t, resp.StatusCode, 202)
		require.JSONEq(t, `{"message":"rule group updated successfully"}`, string(b))
	}

	// Now, let's see how this looks like.
	{
		promRulesURL := fmt.Sprintf("http://%s/api/prometheus/grafana/api/v1/rules", grafanaListedAddr)
		// nolint:gosec
		resp, err := http.Get(promRulesURL)
		require.NoError(t, err)
		t.Cleanup(func() {
			err := resp.Body.Close()
			require.NoError(t, err)
		})
		b, err := ioutil.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode)
		require.JSONEq(t, `
{
	"status": "success",
	"data": {
		"groups": [{
			"name": "arulegroup",
			"file": "default",
			"rules": [{
				"state": "inactive",
				"name": "AlwaysFiring",
				"query": "[{\"datasourceUid\":\"-100\",\"expression\":\"2 + 3 \\u003e 1\",\"intervalMs\":1000,\"maxDataPoints\":100,\"type\":\"math\"}]",
				"duration": 10,
				"annotations": {
					"annotation1": "val1"
				},
				"labels": {
					"label1": "val1"
				},
				"health": "ok",
				"lastError": "",
				"type": "alerting",
				"lastEvaluation": "0001-01-01T00:00:00Z",
				"evaluationTime": 0
			}, {
				"state": "inactive",
				"name": "AlwaysFiringButSilenced",
				"query": "[{\"datasourceUid\":\"-100\",\"expression\":\"2 + 3 \\u003e 1\",\"intervalMs\":1000,\"maxDataPoints\":100,\"type\":\"math\"}]",
				"labels": null,
				"health": "ok",
				"lastError": "",
				"type": "alerting",
				"lastEvaluation": "0001-01-01T00:00:00Z",
				"evaluationTime": 0
			}],
			"interval": 60,
			"lastEvaluation": "0001-01-01T00:00:00Z",
			"evaluationTime": 0
		}]
	}
}`, string(b))
	}

	{
		promRulesURL := fmt.Sprintf("http://%s/api/prometheus/grafana/api/v1/rules", grafanaListedAddr)
		// nolint:gosec
		require.Eventually(t, func() bool {
			resp, err := http.Get(promRulesURL)
			require.NoError(t, err)
			t.Cleanup(func() {
				err := resp.Body.Close()
				require.NoError(t, err)
			})
			b, err := ioutil.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, 200, resp.StatusCode)
			require.JSONEq(t, `
{
	"status": "success",
	"data": {
		"groups": [{
			"name": "arulegroup",
			"file": "default",
			"rules": [{
				"state": "inactive",
				"name": "AlwaysFiring",
				"query": "[{\"datasourceUid\":\"-100\",\"expression\":\"2 + 3 \\u003e 1\",\"intervalMs\":1000,\"maxDataPoints\":100,\"type\":\"math\"}]",
				"duration": 10,
				"annotations": {
					"annotation1": "val1"
				},
				"labels": {
					"label1": "val1"
				},
				"health": "ok",
				"lastError": "",
				"type": "alerting",
				"lastEvaluation": "0001-01-01T00:00:00Z",
				"evaluationTime": 0
			}, {
				"state": "inactive",
				"name": "AlwaysFiringButSilenced",
				"query": "[{\"datasourceUid\":\"-100\",\"expression\":\"2 + 3 \\u003e 1\",\"intervalMs\":1000,\"maxDataPoints\":100,\"type\":\"math\"}]",
				"labels": null,
				"health": "ok",
				"lastError": "",
				"type": "alerting",
				"lastEvaluation": "0001-01-01T00:00:00Z",
				"evaluationTime": 0
			}],
			"interval": 60,
			"lastEvaluation": "0001-01-01T00:00:00Z",
			"evaluationTime": 0
		}]
	}
}`, string(b))
			return true
		}, 18*time.Second, 2*time.Second)
	}
}
