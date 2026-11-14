package jobworker

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestWorkerPassesFragmentIdentityToExistingExecutor(t *testing.T) {
	executor := &fakeExecutor{result: successfulExecution()}
	runner := newTestRunner(t, &fakeScheduler{}, &fakeStore{source: []byte("archive")}, executor)
	job := testJob(time.Now().Add(time.Hour))
	job.ContentKind = "typst"
	job.DocumentEngine = "typst"
	job.RequestMetadata, _ = json.Marshal(requestMetadata{SourceName: "fragments.zip", Entrypoint: "main.typ", DocumentMode: "article", Options: `{"typstFragments":{"templateVersion":"typst-fragment-template/v1","blockIds":["one","two"]}}`})
	_, failure := runner.render(context.Background(), job)
	if failure != nil || len(executor.request.FragmentIDs) != 2 || executor.request.FragmentIDs[1] != "two" || executor.request.DocumentMode != "article" {
		t.Fatalf("fragment identity lost in queue: %+v %+v", executor.request, failure)
	}
}
