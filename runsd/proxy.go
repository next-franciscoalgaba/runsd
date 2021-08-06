// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"k8s.io/klog/v2"
)

type reverseProxy struct {
	projectHash    string
	currentRegion  string
	internalDomain string
}

func newReverseProxy(projectHash, currentRegion, internalDomain string) *reverseProxy {
	return &reverseProxy{
		projectHash:    projectHash,
		currentRegion:  currentRegion,
		internalDomain: internalDomain,
	}
}

const (
	ctxKeyEarlyResponse = `early-response`
)

func (rp *reverseProxy) newReverseProxyHandler(tr http.RoundTripper) http.Handler {
	tokenInject := authenticatingTransport{next: tr}
	transport := loggingTransport{next: tokenInject}

	klog.V(5).Infof("[reverse_proxy] transport=%s", transport)

	return &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1, // to support grpc streaming responses
		Director: func(req *http.Request) {
			klog.V(5).Infof("[director] receive req host=%s", req.Host)
			origHost := req.Host
			if h, p, err := net.SplitHostPort(origHost); err == nil {
				klog.V(6).Infof("discarding port=%v in host=%s", p, origHost)
				origHost = h
			}
			runHost, err := resolveCloudRunHost(rp.internalDomain, origHost, rp.currentRegion, rp.projectHash)
			if err != nil {
				// this only fails due to region code not being registered –which would be handled
				// by the DNS resolver so the request should not come here with an invalid region.
				klog.Warningf("WARN: reverse proxy failed to find a Cloud Run URL for host=%s: %v", req.Host, err)
				resp := &http.Response{
					Request:    req,
					StatusCode: http.StatusInternalServerError,
					Body: ioutil.NopCloser(bytes.NewReader([]byte(
						fmt.Sprintf("runsd doesn't know how to handle host=%q: %v", req.Host, err)))),
				}
				newReq := req.WithContext(context.WithValue(req.Context(), ctxKeyEarlyResponse, resp))
				*req = *newReq
				return
			}
			req.URL.Scheme = "https"
			req.URL.Host = runHost
			req.Host = runHost
			req.Header.Set("host", runHost)
			klog.V(5).Infof("[director] rewrote host=%s to=%s new_url=%q", origHost, runHost, req.URL)
		},
	}
}

func resolveCloudRunHost(internalDomain, hostname, curRegion, projectHash string) (string, error) {
	hostname = strings.ToLower(hostname) // TODO surprisingly not canonicalized by now

	if !strings.Contains(hostname, ".") {
		// in the same region
		rc, ok := cloudRunRegionCodes[curRegion]
		if !ok {
			return "", fmt.Errorf("region %q is not handled", curRegion)
		}
		return mkCloudRunHost(hostname, rc, projectHash), nil
	}

	trimmed := strings.TrimSuffix(hostname, "."+strings.Trim(internalDomain, "."))

	klog.V(5).Infof("number of trimmed=%s", strings.Count(trimmed, "."))

	// Request may be coming from domain in LB
	// Get Cloud Run service name from env K_SERVICE set by GCP
	svc := os.Getenv("K_SERVICE")
	klog.V(5).Infof("service name response=%s", svc)

	client := &http.Client{}
	req, _ := http.NewRequest("GET", "http://metadata.google.internal/computeMetadata/v1/instance/region", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)

	if err != nil {
		return "", fmt.Errorf("Can`t retrieve region for current execution")
	}

	responseBytes, err := ioutil.ReadAll(resp.Body)
	responseString := string(responseBytes)
	responseSplitted := strings.Split(responseString, "/")
	region := responseSplitted[len(responseSplitted)-1]

	klog.V(5).Infof("region response=%s", region)
	rc, ok := cloudRunRegionCodes[region]
	if !ok {
		return "", fmt.Errorf("region %q is not handled", curRegion)
	}

	return mkCloudRunHost(svc, rc, projectHash), nil

}

func mkCloudRunHost(svc, regionCode, projectHash string) string {
	return fmt.Sprintf("%s-%s-%s.a.run.app", svc, projectHash, regionCode)
}

func allowh2c(next http.Handler) http.Handler {
	h2server := &http2.Server{IdleTimeout: time.Second * 60}
	return h2c.NewHandler(next, h2server)
}
