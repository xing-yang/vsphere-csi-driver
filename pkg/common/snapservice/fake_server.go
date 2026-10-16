/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package snapservice

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"

	"github.com/google/uuid"
)

// FakeServer serves the SnapService REST API for every site of a FakeFabric,
// so that CSI controllers on different Supervisors share protection group
// state. Site S is served under /S/api/snapservice; use /S as the client
// endpoint. Admin endpoints under /admin simulate outages and replication.
type FakeServer struct {
	fabric *FakeFabric
	mu     sync.Mutex
	tasks  map[string]*fakeTask
}

type fakeTask struct {
	polls int
	task  wireTask
}

// NewFakeServer returns a FakeServer for the fabric.
func NewFakeServer(fabric *FakeFabric) *FakeServer {
	return &FakeServer{fabric: fabric, tasks: make(map[string]*fakeTask)}
}

// Handler returns the HTTP handler.
func (s *FakeServer) Handler() http.Handler {
	mux := http.NewServeMux()
	p := "/{site}" + apiPrefix
	mux.HandleFunc("POST "+p+"/sessions", s.site(s.createSession))
	mux.HandleFunc("GET "+p+"/sites", s.site(s.listSites))
	mux.HandleFunc("GET "+p+"/clusters/{cluster}/protection-groups", s.site(s.listPGs))
	mux.HandleFunc("POST "+p+"/clusters/{cluster}/protection-groups", s.site(s.createPG))
	mux.HandleFunc("DELETE "+p+"/clusters/{cluster}/protection-groups/{pg}", s.site(s.deletePG))
	mux.HandleFunc("POST "+p+"/clusters/{cluster}/protection-groups/{pg}", s.site(s.pgAction))
	mux.HandleFunc("GET "+p+"/tasks/{task}", s.site(s.getTask))

	mux.HandleFunc("POST /admin/sites/{site}", s.setSiteDown)
	mux.HandleFunc("POST /admin/sync", s.sync)
	mux.HandleFunc("GET /admin/protection-groups", s.dump)
	return mux
}

// site rejects requests for unknown sites.
func (s *FakeServer) site(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		site := r.PathValue("site")
		if !s.fabric.HasSite(site) {
			writeError(w, fmt.Errorf("unknown site %q: %w", site, ErrNotFound))
			return
		}
		h(w, r, site)
	}
}

func (s *FakeServer) createSession(w http.ResponseWriter, r *http.Request, site string) {
	writeJSON(w, http.StatusCreated, "fake-session-"+site)
}

func (s *FakeServer) listSites(w http.ResponseWriter, r *http.Request, site string) {
	peers, err := s.fabric.PairedSites(site)
	if err != nil {
		writeError(w, err)
		return
	}
	sort.Strings(peers)
	out := make([]wireSite, 0, len(peers))
	for _, p := range peers {
		out = append(out, wireSite{Site: p, Name: p})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *FakeServer) listPGs(w http.ResponseWriter, r *http.Request, site string) {
	pgs, err := s.fabric.ListProtectionGroups(site)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]wireProtectionGroup, 0, len(pgs))
	for _, pg := range pgs {
		out = append(out, toWirePG(pg))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func (s *FakeServer) createPG(w http.ResponseWriter, r *http.Request, site string) {
	var spec wireProtectionGroupSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, &wireError{ErrorType: "INVALID_ARGUMENT",
			Messages: []wireMessage{{ID: "snapservice.invalid_argument", DefaultMessage: err.Error()}}})
		return
	}
	_, err := s.fabric.Site(site).CreateProtectionGroup(r.Context(), fromWireSpec(&spec))
	s.startTask(w, nil, err)
}

func (s *FakeServer) deletePG(w http.ResponseWriter, r *http.Request, site string) {
	err := s.fabric.Site(site).DeleteProtectionGroup(r.Context(), r.PathValue("pg"))
	s.startTask(w, nil, err)
}

func (s *FakeServer) pgAction(w http.ResponseWriter, r *http.Request, site string) {
	client := s.fabric.Site(site)
	id := r.PathValue("pg")
	switch action := r.URL.Query().Get("action"); action {
	case "promote":
		var spec wirePromoteSpec
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
				writeJSON(w, http.StatusBadRequest, &wireError{ErrorType: "INVALID_ARGUMENT",
					Messages: []wireMessage{{ID: "snapservice.invalid_argument", DefaultMessage: err.Error()}}})
				return
			}
		}
		res, err := client.Promote(r.Context(), id, spec.Force)
		var result *wirePromoteResult
		if res != nil {
			result = &wirePromoteResult{}
			for _, d := range res.RecoveredDisks {
				result.RecoveredDisks = append(result.RecoveredDisks, wireRecoveredDisk(d))
			}
		}
		s.startTask(w, result, err)
	case "demote":
		s.startTask(w, nil, client.Demote(r.Context(), id))
	case "activate":
		s.startTask(w, nil, client.Activate(r.Context(), id))
	default:
		writeJSON(w, http.StatusBadRequest, &wireError{ErrorType: "INVALID_ARGUMENT",
			Messages: []wireMessage{{ID: "snapservice.invalid_argument",
				DefaultMessage: fmt.Sprintf("unsupported action %q", action)}}})
	}
}

// startTask records a finished operation as a task. A site outage fails the
// request itself; other errors fail the task, as with real vAPI tasks.
func (s *FakeServer) startTask(w http.ResponseWriter, result *wirePromoteResult, err error) {
	if err != nil && errors.Is(err, ErrSiteUnavailable) {
		writeError(w, err)
		return
	}
	t := wireTask{Status: taskStatusSucceeded, Result: result}
	if err != nil {
		we, _ := toWireError(err)
		t = wireTask{Status: taskStatusFailed, Error: we}
	}
	id := "task-" + uuid.NewString()
	s.mu.Lock()
	s.tasks[id] = &fakeTask{task: t}
	s.mu.Unlock()
	writeJSON(w, http.StatusAccepted, id)
}

func (s *FakeServer) getTask(w http.ResponseWriter, r *http.Request, site string) {
	s.mu.Lock()
	t, ok := s.tasks[r.PathValue("task")]
	var out wireTask
	if ok {
		// Report the first poll as running so clients exercise polling.
		if t.polls == 0 {
			out = wireTask{Status: taskStatusRunning}
		} else {
			out = t.task
		}
		t.polls++
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, fmt.Errorf("task %q: %w", r.PathValue("task"), ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *FakeServer) setSiteDown(w http.ResponseWriter, r *http.Request) {
	site := r.PathValue("site")
	if !s.fabric.HasSite(site) {
		writeError(w, fmt.Errorf("unknown site %q: %w", site, ErrNotFound))
		return
	}
	down, err := strconv.ParseBool(r.URL.Query().Get("down"))
	if err != nil {
		http.Error(w, "down must be true or false", http.StatusBadRequest)
		return
	}
	s.fabric.SetSiteDown(site, down)
	w.WriteHeader(http.StatusNoContent)
}

func (s *FakeServer) sync(w http.ResponseWriter, r *http.Request) {
	s.fabric.Sync()
	w.WriteHeader(http.StatusNoContent)
}

// dump returns the protection groups of every reachable site.
func (s *FakeServer) dump(w http.ResponseWriter, r *http.Request) {
	out := map[string][]wireProtectionGroup{}
	s.fabric.mu.Lock()
	sites := make([]string, 0, len(s.fabric.sites))
	for name := range s.fabric.sites {
		sites = append(sites, name)
	}
	s.fabric.mu.Unlock()
	for _, site := range sites {
		pgs, err := s.fabric.ListProtectionGroups(site)
		if err != nil {
			continue
		}
		for _, pg := range pgs {
			out[site] = append(out[site], toWirePG(pg))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func writeError(w http.ResponseWriter, err error) {
	we, code := toWireError(err)
	writeJSON(w, code, we)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
