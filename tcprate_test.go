// Copyright notice. Initial version of the following tests was based on
// the following file from the Go Programming Language core repo:
// https://github.com/golang/go/blob/831f9376d8d730b16fb33dfd775618dffe13ce7a/src/sync/rwmutex_test.go

package tcprate_test

import (
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puzpuzpuz/tcprate"
)

const (
	lowerRateThreshold = 0.95
	upperRateThreshold = 1.05
)

type testDef struct {
	name            string
	limit           int
	perConnLimit    int
	bodySize        int
	numberOfClients int
	numberOfReqs    int
	changeLimit     bool
	changeLimitFn   func(l *tcprate.Listener)
}

func TestListener(t *testing.T) {
	testCases := []testDef{
		{
			name:            "bandwidth limit per server",
			limit:           1024,     // 1 KB/sec
			bodySize:        4 * 1024, // 4 KB
			numberOfClients: 4,
			numberOfReqs:    1,
			changeLimitFn:   func(l *tcprate.Listener) {}, // no-op
		},
		{
			name:            "bandwidth limit per conn",
			perConnLimit:    4 * 1024, // 4 KB/sec
			bodySize:        1024,     // 1 KB
			numberOfClients: 10,
			numberOfReqs:    64,
			changeLimitFn:   func(l *tcprate.Listener) {}, // no-op
		},
		{
			name:            "bandwidth limit per server and conn",
			limit:           16 * 1024, // 16 KB/sec
			perConnLimit:    8 * 1024,  //  8 KB/sec
			bodySize:        32 * 1024, // 32 KB
			numberOfClients: 2,
			numberOfReqs:    3,
			changeLimitFn:   func(l *tcprate.Listener) {}, // no-op
		},
		{
			name:            "change bandwidth limit per server",
			limit:           4 * 1024, // 4 KB/sec
			bodySize:        2 * 1024, // 2 KB
			numberOfClients: 1,
			numberOfReqs:    50,
			changeLimit:     true,
			changeLimitFn: func(l *tcprate.Listener) {
				l.SetLimits(64*1024, 0) // 64 KB/sec
			},
		},
		{
			name:            "change bandwidth limit per conn",
			perConnLimit:    4 * 1024, // 4 KB/sec
			bodySize:        2 * 1024, // 2 KB
			numberOfClients: 1,
			numberOfReqs:    50,
			changeLimit:     true,
			changeLimitFn: func(l *tcprate.Listener) {
				l.SetLimits(0, 64*1024) // 64 KB/sec
			},
		},
		{
			name:            "low bandwidth limit per conn",
			perConnLimit:    1024,      //  1 KB/sec
			bodySize:        16 * 1024, // 16 KB
			numberOfClients: 1,
			numberOfReqs:    1,
			changeLimitFn:   func(l *tcprate.Listener) {}, // no-op
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			totalPerClient, took := testLimits(t, tc)
			if tc.limit > 0 {
				assertServerLimit(t, tc, totalPerClient, took)
			}
			if tc.perConnLimit > 0 {
				assertPerConnLimit(t, tc, totalPerClient, took)
			}
		})
	}
}

func testLimits(t *testing.T, def testDef) (totalPerClient []uint64, took time.Duration) {
	lis, err := net.Listen("tcp", ":")
	if err != nil {
		log.Fatal(err)
	}
	lim := tcprate.NewListener(lis)
	lim.SetLimits(def.limit, def.perConnLimit)
	defer lim.Close()

	s := startTestServer(t, lim, def.bodySize)
	defer s.Close()

	wg := sync.WaitGroup{}
	wg.Add(1)
	cready := make(chan bool, def.numberOfClients)
	cstarted := make(chan bool, def.numberOfClients)
	cdone := make(chan bool, def.numberOfClients)

	addr := "http://localhost:" + strconv.Itoa(lim.Addr().(*net.TCPAddr).Port)
	clients := []*testClient{}
	for i := 0; i < def.numberOfClients; i++ {
		c := &testClient{addr: addr}
		clients = append(clients, c)
		go func() {
			cready <- true
			wg.Wait()
			for j := 0; j < def.numberOfReqs; j++ {
				if err := c.sendRequest(); err != nil {
					t.Error(err)
				}
				if j == 0 {
					cstarted <- true
				}
			}
			cdone <- true
		}()
	}

	for i := 0; i < def.numberOfClients; i++ {
		<-cready
	}
	start := time.Now()
	wg.Done()

	for i := 0; i < def.numberOfClients; i++ {
		<-cstarted
	}
	def.changeLimitFn(lim)

	for i := 0; i < def.numberOfClients; i++ {
		<-cdone
	}

	took = time.Since(start)
	totalPerClient = make([]uint64, def.numberOfClients)
	for i := 0; i < def.numberOfClients; i++ {
		totalPerClient[i] += clients[i].downloaded
	}

	return
}

func assertServerLimit(t *testing.T, def testDef, totalPerClient []uint64, took time.Duration) {
	total := uint64(0)
	for _, t := range totalPerClient {
		total += t
	}
	actualRate := float64(total) / took.Seconds()
	expectedLowerRate := float64(def.limit) * lowerRateThreshold
	expectedUpperRate := float64(def.limit) * upperRateThreshold
	if def.changeLimit {
		if actualRate < expectedUpperRate {
			t.Errorf("rate limit wasn't changed for server: actual=%f, expected at least=%f",
				actualRate, expectedUpperRate)
		}
	} else {
		if actualRate < expectedLowerRate {
			t.Errorf("rate was too low for server: actual=%f, expected at least=%f",
				actualRate, expectedLowerRate)
		}
		if actualRate > expectedUpperRate {
			t.Errorf("rate was too high for server: actual=%f, expected at most=%f",
				actualRate, expectedUpperRate)
		}
	}
}

func assertPerConnLimit(t *testing.T, def testDef, totalPerClient []uint64, took time.Duration) {
	expectedLowerRate := float64(def.perConnLimit) * lowerRateThreshold
	expectedUpperRate := float64(def.perConnLimit) * upperRateThreshold
	for i := 0; i < def.numberOfClients; i++ {
		actualRate := float64(totalPerClient[i]) / took.Seconds()
		if def.changeLimit {
			if actualRate < expectedUpperRate {
				t.Errorf("rate limit wasn't changed for client %d: actual=%f, expected at least=%f",
					i, actualRate, expectedUpperRate)
			}
		} else {
			if actualRate < expectedLowerRate {
				t.Errorf("rate was too low for client: actual=%f, expected at least=%f",
					actualRate, expectedLowerRate)
			}
			if actualRate > expectedUpperRate {
				t.Errorf("rate was too high for client %d: actual=%f, expected at most=%f",
					i, actualRate, expectedUpperRate)
			}
		}
	}
}

func startTestServer(t *testing.T, l *tcprate.Listener, bodySize int) *http.Server {
	h := &testHandler{
		body: generateBody(bodySize),
	}
	s := &http.Server{
		Handler: h,
	}

	go func() {
		if err := s.Serve(l); err != http.ErrServerClosed {
			panic(err)
		}
	}()

	return s
}

func generateBody(size int) []byte {
	// all 'A's for simplicity, could be randomized
	body := make([]byte, size)
	for i := range body {
		body[i] = 'A'
	}
	return body
}

type testHandler struct {
	body []byte
}

func (h *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Write(h.body)
}

type testClient struct {
	client     http.Client
	addr       string
	downloaded uint64 // provides a rough estimate on the total downloaded bytes
}

func (c *testClient) sendRequest() error {
	resp, err := c.client.Get(c.addr)
	if err != nil {
		return err
	}
	bodyLen, _ := io.Copy(ioutil.Discard, resp.Body)
	defer resp.Body.Close()
	atomic.AddUint64(&c.downloaded, uint64(bodyLen))
	headerLen := 0
	for k, vs := range resp.Header {
		headerLen += len(k)
		for _, v := range vs {
			headerLen += len(v)
		}
	}
	atomic.AddUint64(&c.downloaded, uint64(headerLen))
	return nil
}
