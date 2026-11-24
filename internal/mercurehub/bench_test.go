//go:build !nomercure

package mercurehub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// What one mercure_publish() costs in the hub itself, by how many
// subscribers get the update.
func BenchmarkPublish(b *testing.B) {
	for _, subscribers := range []int{0, 100} {
		b.Run(strconv.Itoa(subscribers), func(b *testing.B) {
			hub, err := Start(Declaration{Anonymous: true, Heartbeat: 40, WriteTimeout: 600, DispatchTimeout: 5}, quiet(), nil)
			if err != nil {
				b.Fatal(err)
			}
			defer hub.Stop()
			srv := httptest.NewServer(hub.Routes(http.NotFoundHandler()))
			defer srv.Close()
			for range subscribers {
				resp, err := http.Get(srv.URL + Path + "?match=news")
				if err != nil || resp.StatusCode != 200 {
					b.Fatalf("subscribe: %v, %v", resp, err)
				}
				defer resp.Body.Close()
			}
			update := Update{Topics: []string{"news"}, Data: "0123456789"}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Publish(context.Background(), update); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
