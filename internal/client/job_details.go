package client

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/lydakis/errand/internal/proto"
)

// IsNotFound identifies an absent runner resource, rather than an unavailable runner.
func IsNotFound(err error) bool {
	var response *controlHTTPError
	return errors.As(err, &response) && response.statusCode == http.StatusNotFound
}

type JobDetailResult struct {
	Details proto.JobDetails
	Err     error
}

// InspectJobDetails bounds both concurrency and total time for a listing's
// missing receipts. All requests share one control-request deadline.
func InspectJobDetails(peerURL string, ids []string) []JobDetailResult {
	results := make([]JobDetailResult, len(ids))
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	var wg sync.WaitGroup
	work := make(chan int)
	for range min(8, len(ids)) {
		wg.Go(func() {
			for i := range work {
				results[i].Details, results[i].Err = getJobDetailsContext(ctx, peerURL, ids[i])
			}
		})
	}
	for i := range ids {
		work <- i
	}
	close(work)
	wg.Wait()
	return results
}
