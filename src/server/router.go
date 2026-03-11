package main

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	pb "distributed-llama/generated/inference"
)

const capacityPollTimeout = 1 * time.Second

type scoredClient struct {
	entry        *clientEntry
	slots        int32
	avgLatencyMs float64
}

func pollCapacity(ctx context.Context, clients []*clientEntry) []scoredClient {
	if len(clients) == 0 {
		return nil
	}

	type result struct {
		entry *clientEntry
		resp  *pb.CapacityResponse
	}

	ch := make(chan result, len(clients))
	var wg sync.WaitGroup

	pollCtx, cancel := context.WithTimeout(ctx, capacityPollTimeout)
	defer cancel()

	for _, c := range clients {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.agentClient.QueryCapacity(pollCtx, &pb.CapacityRequest{})
			if err != nil {
				log.Printf("[router] capacity poll failed for %s: %v", c.id[:8], err)
				return
			}
			ch <- result{entry: c, resp: resp}
		}()
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	var scored []scoredClient
	deadline := time.Now().Add(capacityPollTimeout)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		select {
		case r, ok := <-ch:
			if !ok {
				goto done
			}
			if r.resp.EstimatedSlots > 0 {
				scored = append(scored, scoredClient{
					entry:        r.entry,
					slots:        r.resp.EstimatedSlots,
					avgLatencyMs: r.entry.getAvgLatency(),
				})
			} else {
				log.Printf("[router] client %s not ready (slots=%d)", r.entry.id[:8], r.resp.EstimatedSlots)
			}
		case <-time.After(remaining):
			goto done
		}
	}
done:

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].slots != scored[j].slots {
			return scored[i].slots > scored[j].slots
		}
		return scored[i].avgLatencyMs < scored[j].avgLatencyMs
	})

	return scored
}

func routeRequest(scored []scoredClient) *clientEntry {
	if len(scored) == 0 {
		return nil
	}
	return scored[0].entry
}
