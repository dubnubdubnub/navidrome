package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/server"
	"github.com/navidrome/navidrome/server/events"
	"github.com/navidrome/navidrome/server/subsonic/responses"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type noopInsights struct{}

func (noopInsights) Run(context.Context)                       {}
func (noopInsights) LastRun(context.Context) (time.Time, bool) { return time.Time{}, false }

// These go through server.New and MountRouter, the same route chain cmd builds, so the Subsonic
// limiter sees whatever realIPMiddleware resolved. Reordering the default middlewares or mounting
// the Subsonic API outside the server's router breaks them.
var _ = Describe("Login limiting by the client IP from a trusted proxy's header", func() {
	var app http.Handler

	BeforeEach(func() {
		setupTestDB()
		conf.Server.AuthRequestLimit = 3
		conf.Server.AuthWindowLength = time.Minute
		conf.Server.ClientIPHeader.Name = "CF-Connecting-IP"
		conf.Server.ClientIPHeader.TrustedProxies = "127.0.0.1/32"

		srv := server.New(ds, events.NoopBroker(), noopInsights{})
		srv.MountRouter("Subsonic API", consts.URLPathSubsonicAPI, newRouter())
		app = srv
	})

	ping := func(peer, clientIP, password string) *responses.Subsonic {
		q := url.Values{
			"u": {adminUser.UserName}, "p": {password},
			"v": {"1.16.1"}, "c": {"test-client"}, "f": {"json"},
		}
		r := httptest.NewRequest("GET", consts.URLPathSubsonicAPI+"/ping?"+q.Encode(), nil)
		r.RemoteAddr = peer
		r.Header.Set("CF-Connecting-IP", clientIP)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return parseJSONResponse(w)
	}
	failed := func(resp *responses.Subsonic) bool {
		return resp.Status == responses.StatusFailed && resp.Error != nil && resp.Error.Code == responses.ErrorAuthenticationFail
	}

	It("passes a correct login through the full chain", func() {
		Expect(ping("127.0.0.1:51000", "203.0.113.1", "password").Status).To(Equal(responses.StatusOK))
	})

	It("keeps a separate bucket per header IP when the peer is the trusted forwarder", func() {
		for range 3 {
			Expect(failed(ping("127.0.0.1:51000", "203.0.113.1", "WRONG"))).To(BeTrue())
		}
		Expect(failed(ping("127.0.0.1:51001", "203.0.113.1", "password"))).To(BeTrue(), "the failing client is locked out")
		Expect(ping("127.0.0.1:51002", "198.51.100.2", "password").Status).To(Equal(responses.StatusOK),
			"another client with the same username is not")
	})

	It("keeps one bucket per peer when an untrusted peer rotates the header", func() {
		for i, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
			Expect(failed(ping("10.42.0.7:40000", ip, "WRONG"))).To(BeTrue(), "attempt %d", i)
		}
		Expect(failed(ping("10.42.0.7:40001", "198.51.100.2", "password"))).To(BeTrue(),
			"a fresh header IP does not give an untrusted peer a fresh bucket")
		Expect(ping("127.0.0.1:51000", "198.51.100.2", "password").Status).To(Equal(responses.StatusOK),
			"the trusted forwarder's clients are unaffected")
	})
})
