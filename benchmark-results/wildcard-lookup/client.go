package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const docs = 16

var scenarios = []string{"direct10", "direct1000", "direct5000", "chain5000", "group5000", "concrete"}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
func relation(resourceType, resourceID, relation, subjectType, subjectID string) *v1.Relationship {
	return &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: relation, Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: subjectType, ObjectId: subjectID}}}
}
func fixture(scenario string) (string, int, bool) {
	switch scenario {
	case "direct10":
		return "direct", 10, true
	case "direct1000":
		return "direct", 1000, true
	case "direct5000":
		return "direct", 5000, true
	case "chain5000":
		return "chained", 5010, true
	case "group5000":
		return "inherited", 5000, true
	case "concrete":
		return "concrete", 500, false
	default:
		panic("unknown scenario: " + scenario)
	}
}
func seed(ctx context.Context, conn *grpc.ClientConn, tokenPath string) {
	_, err := v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: `
 definition user {}
 definition group {
  relation member: user
 }
 definition document {
  relation public: user:*
  relation banned: user
  relation suspended: user
  relation blocklist: group
  relation member: user
  permission direct = public - banned
  permission chained = public - banned - suspended
  permission inherited = public - blocklist->member
  permission concrete = member - banned
 }
 `})
	check(err)
	client := v1.NewPermissionsServiceClient(conn)
	batch := make([]*v1.RelationshipUpdate, 0, 1000)
	var token *v1.ZedToken
	written := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		resp, err := client.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: batch})
		check(err)
		token = resp.WrittenAt
		written += len(batch)
		batch = batch[:0]
	}
	add := func(r *v1.Relationship) {
		batch = append(batch, &v1.RelationshipUpdate{Operation: v1.RelationshipUpdate_OPERATION_TOUCH, Relationship: r})
		if len(batch) == 1000 {
			flush()
		}
	}
	for _, scenario := range scenarios {
		for d := range docs {
			id := fmt.Sprintf("%s_%d", scenario, d)
			_, count, wildcard := fixture(scenario)
			if wildcard {
				add(relation("document", id, "public", "user", "*"))
			}
			switch scenario {
			case "group5000":
				add(relation("document", id, "blocklist", "group", id))
				for i := range count {
					add(relation("group", id, "member", "user", fmt.Sprintf("u%d", i)))
				}
			case "concrete":
				for i := range 1000 {
					add(relation("document", id, "member", "user", fmt.Sprintf("u%d", i)))
				}
				for i := range 500 {
					add(relation("document", id, "banned", "user", fmt.Sprintf("u%d", i)))
				}
			default:
				for i := range count {
					rel := "banned"
					if scenario == "chain5000" && i >= 5000 {
						rel = "suspended"
					}
					add(relation("document", id, rel, "user", fmt.Sprintf("u%d", i)))
				}
			}
		}
	}
	flush()
	check(os.WriteFile(tokenPath, []byte(token.Token), 0600))
	fmt.Printf("seeded %d relationships across %d documents\n", written, docs*len(scenarios))
}
func lookup(ctx context.Context, client v1.PermissionsServiceClient, scenario string, doc int, consistency *v1.Consistency) (time.Duration, error) {
	permission, expected, wildcard := fixture(scenario)
	start := time.Now()
	stream, err := client.LookupSubjects(ctx, &v1.LookupSubjectsRequest{Resource: &v1.ObjectReference{ObjectType: "document", ObjectId: fmt.Sprintf("%s_%d", scenario, doc)}, Permission: permission, SubjectObjectType: "user", Consistency: consistency})
	if err != nil {
		return time.Since(start), err
	}
	results := make([]*v1.LookupSubjectsResponse, 0, 1)
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return time.Since(start), err
		}
		results = append(results, resp)
	}
	latency := time.Since(start)
	seen := make([]bool, expected)
	if wildcard && len(results) != 1 {
		return latency, fmt.Errorf("expected one wildcard, got %d subjects", len(results))
	}
	for _, resp := range results {
		if resp.Subject == nil || resp.Subject.Permissionship != v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_HAS_PERMISSION {
			return latency, fmt.Errorf("unexpected subject permissionship: %v", resp.Subject)
		}
		subjects := resp.ExcludedSubjects
		if wildcard {
			if resp.Subject.SubjectObjectId != "*" {
				return latency, fmt.Errorf("expected wildcard")
			}
		} else {
			if len(subjects) > 0 {
				return latency, fmt.Errorf("unexpected exclusions")
			}
			subjects = []*v1.ResolvedSubject{resp.Subject}
		}
		for _, sub := range subjects {
			if sub.Permissionship != v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_HAS_PERMISSION || sub.PartialCaveatInfo != nil {
				return latency, fmt.Errorf("unexpected exclusion caveat")
			}
			if !strings.HasPrefix(sub.SubjectObjectId, "u") {
				return latency, fmt.Errorf("unexpected subject ID")
			}
			n, err := strconv.Atoi(sub.SubjectObjectId[1:])
			if !wildcard {
				n -= 500
			}
			if err != nil || n < 0 || n >= expected || seen[n] {
				return latency, fmt.Errorf("invalid or duplicate subject: %s", sub.SubjectObjectId)
			}
			seen[n] = true
		}
	}
	for _, found := range seen {
		if !found {
			return latency, fmt.Errorf("missing expected subject")
		}
	}
	return latency, nil
}
func metrics(addr string) map[string]float64 {
	resp, err := http.Get(addr + "/metrics")
	check(err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		panic(resp.Status)
	}
	out := map[string]float64{}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		if strings.Contains(name, "{") {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err == nil {
			out[name] = value
		}
	}
	check(scanner.Err())
	return out
}
func main() {
	action := flag.String("action", "run", "seed, verify, or run")
	target := flag.String("target", "127.0.0.1:55051", "gRPC address")
	metricAddr := flag.String("metrics", "http://127.0.0.1:59090", "metrics address")
	scenario := flag.String("scenario", "direct5000", "fixture")
	mode := flag.String("mode", "snapshot", "snapshot, fresh, or churn")
	concurrency := flag.Int("concurrency", 8, "parallel clients")
	rps := flag.Int("rps", 0, "fixed arrivals per second; zero runs closed loop")
	duration := flag.Duration("duration", 3*time.Second, "measurement duration")
	tokenPath := flag.String("token", "seed-token.txt", "seed token file")
	flag.Parse()
	conn, err := grpc.NewClient(*target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(32<<20)))
	check(err)
	defer conn.Close()
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer local-benchmark-key")
	if *action == "seed" {
		seed(ctx, conn, *tokenPath)
		return
	}
	token, err := os.ReadFile(*tokenPath)
	check(err)
	consistency := &v1.Consistency{Requirement: &v1.Consistency_AtExactSnapshot{AtExactSnapshot: &v1.ZedToken{Token: string(token)}}}
	if *mode != "snapshot" {
		consistency = &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}}
	}
	client := v1.NewPermissionsServiceClient(conn)
	if *action == "verify" {
		for _, s := range scenarios {
			for d := range docs {
				_, err := lookup(ctx, client, s, d, consistency)
				check(err)
			}
		}
		fmt.Println("all 96 document lookups returned the exact expected subjects and permissionships")
		return
	}
	for d := range docs {
		_, err := lookup(ctx, client, *scenario, d, consistency)
		check(err)
	}
	time.Sleep(100 * time.Millisecond)
	var writeCount atomic.Int64
	var writeErrors atomic.Int64
	writeCtx, stopWrites := context.WithCancel(ctx)
	var writers sync.WaitGroup
	if *mode == "churn" {
		writers.Add(1)
		go func() {
			defer writers.Done()
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-writeCtx.Done():
					return
				case <-ticker.C:
					_, err := client.WriteRelationships(writeCtx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{Operation: func() v1.RelationshipUpdate_Operation {
						if writeCount.Load()%2 == 0 {
							return v1.RelationshipUpdate_OPERATION_TOUCH
						}
						return v1.RelationshipUpdate_OPERATION_DELETE
					}(), Relationship: relation("document", "write_marker", "member", "user", "writer")}}})
					if err != nil {
						if writeCtx.Err() == nil {
							writeErrors.Add(1)
						}
					} else {
						writeCount.Add(1)
					}
				}
			}
		}()
	}
	before := metrics(*metricAddr)
	start := time.Now()
	deadline := start.Add(*duration)
	var requests atomic.Int64
	var wg sync.WaitGroup
	type requestJob struct {
		index     int64
		scheduled time.Time
	}
	var jobs chan requestJob
	if *rps > 0 {
		jobs = make(chan requestJob, int(duration.Seconds()*float64(*rps)))
		go func() {
			defer close(jobs)
			for i := int64(0); i < int64(duration.Seconds()*float64(*rps)); i++ {
				scheduled := start.Add(time.Duration(i) * time.Second / time.Duration(*rps))
				time.Sleep(time.Until(scheduled))
				jobs <- requestJob{i, scheduled}
			}
		}()
	}
	samples := make([][]float64, *concurrency)
	failures := make([][]string, *concurrency)
	for worker := range *concurrency {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				var job requestJob
				if jobs != nil {
					var ok bool
					job, ok = <-jobs
					if !ok {
						break
					}
				} else if !time.Now().Before(deadline) {
					break
				}
				n := requests.Add(1) - 1
				var queued time.Duration
				if jobs != nil {
					n = job.index
					queued = time.Since(job.scheduled)
				}
				requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				latency, err := lookup(requestCtx, client, *scenario, int(n%docs), consistency)
				latency += queued
				cancel()
				if err != nil {
					failures[w] = append(failures[w], err.Error())
				} else {
					samples[w] = append(samples[w], float64(latency.Nanoseconds())/1e6)
				}
			}
		}(worker)
	}
	wg.Wait()
	elapsed := time.Since(start)
	stopWrites()
	writers.Wait()
	after := metrics(*metricAddr)
	combined := []float64{}
	errs := []string{}
	for w := range *concurrency {
		combined = append(combined, samples[w]...)
		errs = append(errs, failures[w]...)
	}
	sort.Float64s(combined)
	quantile := func(q float64) float64 {
		if len(combined) == 0 {
			return 0
		}
		return combined[int(math.Ceil(q*float64(len(combined))))-1]
	}
	sum := 0.0
	for _, v := range combined {
		sum += v
	}
	delta := map[string]float64{}
	for name, end := range after {
		if value, ok := before[name]; ok {
			delta[name] = end - value
		}
	}
	result := map[string]any{"offered_rps": *rps, "scenario": *scenario, "mode": *mode, "concurrency": *concurrency, "requests": requests.Load(), "successes": len(combined), "errors": len(errs), "error_examples": errs, "seconds": elapsed.Seconds(), "requests_per_second": float64(len(combined)) / elapsed.Seconds(), "p50_ms": quantile(.5), "p95_ms": quantile(.95), "p99_ms": quantile(.99), "mean_ms": sum / float64(max(1, len(combined))), "writes": writeCount.Load(), "write_errors": writeErrors.Load(), "metrics_delta": delta, "metrics_after": after}
	check(json.NewEncoder(os.Stdout).Encode(result))
	if len(errs) > 0 || writeErrors.Load() > 0 {
		os.Exit(1)
	}
}
