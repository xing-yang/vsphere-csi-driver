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

// fake-snapservice serves an in-memory SnapService REST API for several
// paired sites, so that CSI controllers on different Supervisors can test
// volume replication without SnapService. It is for testing only: there is no
// authentication and no data is replicated.
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/snapservice"
)

func main() {
	listen := flag.String("listen", ":8080", "address to listen on")
	sites := flag.String("sites", "site-a,site-b", "comma-separated site names; all sites are paired")
	autoSync := flag.Bool("auto-sync", true,
		"mark replicas synced immediately; when false, POST /admin/sync completes replication")
	registerRecovered := flag.Bool("register-recovered", false,
		"report promoted FCDs as already registered with their source ID")
	certFile := flag.String("tls-cert", "", "TLS certificate file; serves plain HTTP when empty")
	keyFile := flag.String("tls-key", "", "TLS key file")
	flag.Parse()

	names := strings.Split(*sites, ",")
	fabric := snapservice.NewFakeFabric(names,
		snapservice.WithAutoSync(*autoSync),
		snapservice.WithRegisterRecovered(*registerRecovered))
	server := &http.Server{
		Addr:              *listen,
		Handler:           logRequests(snapservice.NewFakeServer(fabric).Handler()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	for _, s := range names {
		log.Printf("site %q: endpoint http(s)://<host>%s/%s", s, *listen, s)
	}
	var err error
	if *certFile != "" {
		err = server.ListenAndServeTLS(*certFile, *keyFile)
	} else {
		err = server.ListenAndServe()
	}
	log.Fatal(err)
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.RequestURI(), time.Since(start))
	})
}
