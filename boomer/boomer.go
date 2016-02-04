// Copyright 2014 Google Inc. All Rights Reserved.
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

// Package boomer provides commands to run load tests and display results.
package boomer

import (
	"crypto/tls"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"

	"github.com/rakyll/pb"

	"bytes"
	"math/rand"
	"text/template"
	"time"
)

type result struct {
	err           error
	statusCode    int
	duration      time.Duration
	contentLength int64
}

type Boomer struct {
	// Request URL to check load with
	RequestURL string

	// Basic authentication username parameter is needed
	AuthUsername string

	// Basic authentication password parameter is needed
	AuthPassword string

	// HTTP request body content
	RequestBody string

	// HTTP Method (GET, POST, PUT, DELETE)
	Method string

	// HTTP request header
	Header http.Header

	// N is the total number of requests to make.
	N int

	// C is the concurrency level, the number of concurrent workers to run.
	C int

	// Timeout in seconds.
	Timeout int

	// Qps is the rate limit.
	Qps int

	// AllowInsecure is an option to allow insecure TLS/SSL certificates.
	AllowInsecure bool

	// DisableCompression is an option to disable compression in response
	DisableCompression bool

	// DisableKeepAlives is an option to prevents re-use of TCP connections between different HTTP requests
	DisableKeepAlives bool

	// Output represents the output type. If "csv" is provided, the
	// output will be dumped as a csv stream.
	Output string

	// ProxyAddr is the address of HTTP proxy server in the format on "host:port".
	// Optional.
	ProxyAddr *url.URL

	// ReadAll determines whether the body of the response needs
	// to be fully consumed.
	ReadAll bool

	bar     *pb.ProgressBar
	results chan *result
}

var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func randSeq(n int) string {
	rand.Seed(time.Now().UnixNano())
	b := make([]rune, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func (b *Boomer) startProgress() {
	if b.Output != "" {
		return
	}
	b.bar = pb.New(b.N)
	b.bar.Format("Bom !")
	b.bar.Start()
}

func (b *Boomer) finalizeProgress() {
	if b.Output != "" {
		return
	}
	b.bar.Finish()
}

func (b *Boomer) incProgress() {
	if b.Output != "" {
		return
	}
	b.bar.Increment()
}

// Run makes all the requests, prints the summary. It blocks until
// all work is done.
func (b *Boomer) Run() {
	b.results = make(chan *result, b.N)
	b.startProgress()

	start := time.Now()
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		<-c
		// TODO(jbd): Progress bar should not be finalized.
		newReport(b.N, b.results, b.Output, time.Now().Sub(start)).finalize()
	}()

	b.runWorkers()
	b.finalizeProgress()
	newReport(b.N, b.results, b.Output, time.Now().Sub(start)).finalize()
	close(b.results)
}

func (b *Boomer) runWorker(wg *sync.WaitGroup, ch chan *http.Request) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: b.AllowInsecure,
		},
		DisableCompression: b.DisableCompression,
		DisableKeepAlives:  b.DisableKeepAlives,
		// TODO(jbd): Add dial timeout.
		TLSHandshakeTimeout: time.Duration(b.Timeout) * time.Millisecond,
		Proxy:               http.ProxyURL(b.ProxyAddr),
	}
	client := &http.Client{Transport: tr}
	for req := range ch {
		s := time.Now()

		var code int
		var size int64

		resp, err := client.Do(req)
		if err == nil {
			size = resp.ContentLength
			code = resp.StatusCode
			if b.ReadAll {
				_, err = io.Copy(ioutil.Discard, resp.Body)
			}
			resp.Body.Close()
		}

		wg.Done()
		b.incProgress()
		b.results <- &result{
			statusCode:    code,
			duration:      time.Now().Sub(s),
			err:           err,
			contentLength: size,
		}
	}
}

func (b *Boomer) runWorkers() {
	var wg sync.WaitGroup
	wg.Add(b.N)

	var throttle <-chan time.Time
	if b.Qps > 0 {
		throttle = time.Tick(time.Duration(1e6/(b.Qps)) * time.Microsecond)
	}

	jobsch := make(chan *http.Request, b.N)
	for i := 0; i < b.C; i++ {
		go b.runWorker(&wg, jobsch)
	}

	for i := 0; i < b.N; i++ {
		if b.Qps > 0 {
			<-throttle
		}
		jobsch <- createRequest(b.RequestURL, b.Method, b.AuthUsername, b.AuthPassword, b.Header, b.RequestBody)
	}
	close(jobsch)
	wg.Wait()
}

func createRequest(url string, method string, user string, passwd string, header http.Header, body string) *http.Request {
	// and replace templates with random values
	fUrl := evalTmpl(url)
	req, _ := http.NewRequest(method, fUrl, nil)

	// deep copy of the Header
	req.Header = make(http.Header, len(header))
	for k, s := range header {
		req.Header[k] = append([]string(nil), s...)
	}

	if user != "" || passwd != "" {
		req.SetBasicAuth(user, passwd)
	}

	req.Body = ioutil.NopCloser(strings.NewReader(evalTmpl(body)))
	return req
}

func evalTmpl(s string) string {
	var result bytes.Buffer
	tmpl, _ := template.New("base").Funcs(template.FuncMap{"rand": randSeq}).Parse(s)
	tmpl.Execute(&result, "")
	return result.String()
}
