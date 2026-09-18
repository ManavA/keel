package jobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloudRunTrigger_Run_Accepted(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	trig := &CloudRunTrigger{
		projectID: "proj",
		region:    "us-west1",
		client:    srv.Client(),
		baseURL:   srv.URL,
	}

	err := trig.Run(context.Background(), "reindex")
	require.NoError(t, err)
	assert.Contains(t, gotPath, "/jobs/reindex:run")
	assert.Contains(t, gotPath, "/namespaces/proj/")
}

func TestCloudRunTrigger_Run_NonSuccessStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("permission denied"))
	}))
	defer srv.Close()

	trig := &CloudRunTrigger{
		projectID: "proj",
		region:    "us-west1",
		client:    srv.Client(),
		baseURL:   srv.URL,
	}

	err := trig.Run(context.Background(), "reindex")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), "permission denied")
}

func TestNewCloudRunTrigger_RequiresProjectAndRegion(t *testing.T) {
	_, err := NewCloudRunTrigger(context.Background(), CloudRunTriggerOptions{})
	require.Error(t, err)
}

func TestNoopTrigger_Run_NeverErrors(t *testing.T) {
	var trig NoopTrigger
	assert.NoError(t, trig.Run(context.Background(), "anything"))
}
