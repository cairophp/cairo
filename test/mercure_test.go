package test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	mercureKey = "a-key-of-the-tests-that-is-long-enough-for-HS256"
	newsTopic  = "https://example.com/news"
)

// runMercure runs examples/mercure on one PHP thread and returns its base
// URL, and a function that stops it as a deploy does and returns its log.
func runMercure(t *testing.T, env ...string) (base string, stop func() string) {
	t.Helper()
	addr := freeAddr()
	logs := &syncBuffer{}
	cmd := exec.Command(cairoBin, "serve", "../examples/mercure/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(append(os.Environ(), "CAIRO_HTTP="+addr, "MERCURE_URL=http://"+addr, "MERCURE_KEY="+mercureKey), env...)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop = sync.OnceValue(func() string {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
		return logs.String()
	})
	t.Cleanup(func() {
		http.DefaultClient.CloseIdleConnections()
		if log := stop(); t.Failed() {
			t.Logf("server log:\n%s", log)
		}
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		// A binary built in another way than the Makefile's has no hub
		// (nomercure), or one that takes no clients from before 1.0.
		for _, missing := range []string{"built without the Mercure hub", "needs a cairo built with the tags"} {
			if strings.Contains(logs.String(), missing) {
				t.Skipf("this binary: %s", missing)
			}
		}
		t.Fatalf("%v\n%s", err, logs.String())
	}
	return "http://" + addr, stop
}

// mercureToken signs a token as an issuer does: the claims with HS256.
func mercureToken(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	part := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header["alg"] = "HS256"
	signed := part(header) + "." + part(claims)
	mac := hmac.New(sha256.New, []byte(mercureKey))
	mac.Write([]byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// accessToken is a Mercure 1.0 token of the example's issuer for one action
// on some topics ("*" for all).
func accessToken(t *testing.T, base, action string, topics ...string) string {
	t.Helper()
	matches := make([]map[string]string, len(topics))
	for i, topic := range topics {
		matches[i] = map[string]string{"match": topic}
	}
	return mercureToken(t, map[string]any{"typ": "at+jwt"}, accessClaims(base, action, matches))
}

// accessClaims are the claims of a token that the example's hub takes.
func accessClaims(base, action string, matches []map[string]string) map[string]any {
	return map[string]any{
		"iss": base, "aud": base + "/.well-known/mercure", "sub": "tests", "client_id": "tests", "jti": action,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		"authorization_details": []map[string]any{{
			"type": "https://mercure.rocks/authorization-detail", "actions": []string{action}, "topics": matches,
		}},
	}
}

// subscriber is a client that listens to the hub.
type subscriber struct {
	t    *testing.T
	resp *http.Response
	body *bufio.Reader
}

// subscribe opens a subscription with the query as given and waits for its
// head. The hub sends the head at once: from then on the subscriber misses
// nothing.
func subscribe(t *testing.T, base, query, token string) *subscriber {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/.well-known/mercure?"+query, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		t.Fatalf("subscribe %s: status %d, %v: %s", query, resp.StatusCode, resp.Header, b)
	}
	return &subscriber{t: t, resp: resp, body: bufio.NewReader(resp.Body)}
}

func match(topics ...string) string {
	q := url.Values{}
	for _, topic := range topics {
		q.Add("match", topic)
	}
	return q.Encode()
}

// event is the next event of the stream as its fields, without the
// comments the hub sends in between. It fails the test after 5 seconds.
func (s *subscriber) event() map[string]string {
	s.t.Helper()
	got := make(chan map[string]string, 1)
	go func() {
		fields := map[string]string{}
		for {
			line, err := s.body.ReadString('\n')
			if err != nil {
				got <- nil
				return
			}
			line = strings.TrimSuffix(line, "\n")
			name, value, isField := strings.Cut(line, ": ")
			switch {
			case line == "" && len(fields) > 0:
				got <- fields
				return
			case isField && name != "":
				fields[name] = value
			}
		}
	}()
	select {
	case fields := <-got:
		if fields == nil {
			s.t.Fatal("the subscription ended")
		}
		return fields
	case <-time.After(5 * time.Second):
		s.t.Fatal("no event within 5 s")
		return nil
	}
}

// publish asks the example to publish the body with mercure_publish() and
// returns the update's id.
func publish(t *testing.T, base, query, data string) string {
	t.Helper()
	resp, err := http.Post(base+"/publish?"+query, "text/plain", strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var answer struct {
		ID string `json:"id"`
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || json.Unmarshal(b, &answer) != nil || answer.ID == "" {
		t.Fatalf("publish %s: status %d: %s", query, resp.StatusCode, b)
	}
	return answer.ID
}

// What PHP publishes reaches every subscriber of the topic, with the id
// that mercure_publish() returned, and nobody else.
func TestMercurePublishReachesSubscribers(t *testing.T) {
	base, _ := runMercure(t)
	news, both := subscribe(t, base, match(newsTopic), ""), subscribe(t, base, match(newsTopic, "https://example.com/sport"), "")
	other := subscribe(t, base, match("https://example.com/weather"), "")

	id := publish(t, base, "type=headline", "first\nsecond line")
	for _, s := range []*subscriber{news, both} {
		if ev := s.event(); ev["id"] != id || ev["data"] != "second line" || ev["event"] != "headline" {
			t.Errorf("event = %v, want id %s", ev, id)
		}
	}
	weather := publish(t, base, "topic="+url.QueryEscape("https://example.com/weather"), "rain")
	if ev := other.event(); ev["id"] != weather || ev["data"] != "rain" {
		t.Errorf("the other topic's subscriber got %v", ev)
	}
	sport := publish(t, base, "topic="+url.QueryEscape("https://example.com/sport")+"&id=score-1", "1:0")
	if ev := both.event(); sport != "score-1" || ev["id"] != "score-1" || ev["data"] != "1:0" {
		t.Errorf("an update with its own id: %v (%s)", ev, sport)
	}

	// A handler may publish after it answered (Context::defer()).
	resp, err := http.Post(base+"/publish?after=1", "text/plain", strings.NewReader("later"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ev := news.event(); resp.StatusCode != 202 || ev["data"] != "later" {
		t.Errorf("an update published after the answer: status %d, event %v", resp.StatusCode, ev)
	}
}

// An update the protocol does not allow is the caller's mistake: a
// ValueError, not an update.
func TestMercureRefusesInvalidUpdates(t *testing.T) {
	base, _ := runMercure(t)
	for name, query := range map[string]string{
		"the wildcard":      "topic=*",
		"the hub's own":     "topic=" + url.QueryEscape("/.well-known/mercure/subscriptions"),
		"a reserved id":     "id=earliest",
		"the reserved type": "type=mercure",
	} {
		resp, err := http.Post(base+"/publish?"+query, "text/plain", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 422 || !strings.HasPrefix(string(b), "mercure_publish(): ") {
			t.Errorf("%s: status %d: %s", name, resp.StatusCode, b)
		}
	}
}

// A private update goes to the subscribers whose token names its topic. A
// program publishes with a POST and a token of its own; without one, and
// with a subscriber's, it is refused.
func TestMercureTokens(t *testing.T) {
	base, _ := runMercure(t)
	hub := base + "/.well-known/mercure"
	anonymous := subscribe(t, base, match(newsTopic), "")
	allowed := subscribe(t, base, match(newsTopic), accessToken(t, base, "subscribe", newsTopic))
	elsewhere := subscribe(t, base, match(newsTopic), accessToken(t, base, "subscribe", "https://example.com/sport"))

	secret := publish(t, base, "private=1", "for some")
	if ev := allowed.event(); ev["id"] != secret || ev["data"] != "for some" {
		t.Errorf("the allowed subscriber got %v", ev)
	}
	open := publish(t, base, "", "for all")
	for name, s := range map[string]*subscriber{"anonymous": anonymous, "a token for another topic": elsewhere, "allowed": allowed} {
		if ev := s.event(); ev["id"] != open {
			t.Errorf("%s: got %v, want the public update only", name, ev)
		}
	}

	post := func(token string) (int, string) {
		form := url.Values{"topic": {newsTopic}, "data": {"over HTTP"}}
		req, _ := http.NewRequest("POST", hub, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if status, _ := post(""); status != 401 {
		t.Errorf("a POST without a token: status %d, want 401", status)
	}
	if status, _ := post(accessToken(t, base, "subscribe", "*")); status != 403 {
		t.Errorf("a POST with a subscriber's token: status %d, want 403", status)
	}
	// A token must be this issuer's, for this hub, still valid and signed
	// with the key: each of these would publish if one check were missing.
	all := []map[string]string{{"match": "*"}}
	foreign := strings.Replace(base, "127.0.0.1", "localhost", 1)
	forged := func(change func(claims map[string]any)) string {
		claims := accessClaims(base, "publish", all)
		change(claims)
		return mercureToken(t, map[string]any{"typ": "at+jwt"}, claims)
	}
	otherKey := accessToken(t, base, "publish", "*")
	otherKey = otherKey[:strings.LastIndex(otherKey, ".")+1] + base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size))
	for name, token := range map[string]string{
		"another hub's token":         accessToken(t, foreign, "publish", "*"),
		"a token for another hub":     forged(func(c map[string]any) { c["aud"] = "https://other.example/.well-known/mercure" }),
		"a token without an audience": forged(func(c map[string]any) { delete(c, "aud") }),
		"another issuer's token":      forged(func(c map[string]any) { c["iss"] = "https://other.example" }),
		"an expired token":            forged(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
		"a token of another key":      otherKey,
	} {
		if status, _ := post(token); status != 401 {
			t.Errorf("a POST with %s: status %d, want 401", name, status)
		}
	}
	status, id := post(accessToken(t, base, "publish", "*"))
	if ev := anonymous.event(); status != 200 || ev["id"] != id || ev["data"] != "over HTTP" {
		t.Errorf("a POST with a publisher's token: status %d, id %q, event %v", status, id, ev)
	}

	// The hub answers for its public URL only.
	req, _ := http.NewRequest("GET", hub+"?"+match(newsTopic), nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("a request for another host: status %d, want 421", resp.StatusCode)
	}
}

// With subscriptions(), the hub says who subscribes to what, to a token
// that may ask, and its metrics join Cairo's on the server that has them.
func TestMercureSubscriptionsAndMetrics(t *testing.T) {
	ops := freeAddr()
	base, _ := runMercure(t, "MERCURE_SUBSCRIPTIONS=1", "CAIRO_OPS="+ops)
	subscribe(t, base, match(newsTopic), "")
	list := base + "/.well-known/mercure/subscriptions"
	get := func(token string) (int, string) {
		req, _ := http.NewRequest("GET", list, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if status, _ := get(""); status != 401 {
		t.Errorf("the subscriptions without a token: status %d, want 401", status)
	}
	if status, _ := get(accessToken(t, base, "subscribe", newsTopic)); status != 403 {
		t.Errorf("the subscriptions with a token for a topic: status %d, want 403", status)
	}
	status, body := get(accessToken(t, base, "subscribe", "/.well-known/mercure/subscriptions"))
	if status != 200 || !strings.Contains(body, newsTopic) {
		t.Errorf("the subscriptions: status %d: %s", status, body)
	}

	publish(t, base, "", "counted")
	metrics := fetchBody(t, "http://"+ops+"/metrics")
	for _, want := range []string{"mercure_subscribers_connected", "mercure_updates_total", "cairo_http_request_duration_seconds"} {
		if !strings.Contains(metrics, want) {
			t.Errorf("the metrics have no %s", want)
		}
	}
	// A subscriber is no request of the app: it would never end, or count
	// as hours of one.
	if strings.Contains(metrics, "/.well-known/mercure") {
		t.Errorf("the hub's requests are among the app's metrics:\n%s", metrics)
	}
}

// Subscribers are Go's: a hundred of them hold no PHP memory, outlast the
// time limit of requests, and fifty requests that each wait a second before
// they publish share the one PHP thread.
func TestMercureSubscribersHoldNoPHP(t *testing.T) {
	base, _ := runMercure(t, "CAIRO_TIMEOUT=3")
	memory := func() int {
		var m struct {
			PHP int `json:"php"`
		}
		_ = json.Unmarshal([]byte(fetchBody(t, base+"/memory")), &m)
		return m.PHP
	}
	before := memory()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 200, MaxConnsPerHost: 200}}
	t.Cleanup(client.CloseIdleConnections)
	const listeners, publishers = 100, 50
	subs := make([]*subscriber, listeners)
	for i := range subs {
		subs[i] = subscribe(t, base, match(newsTopic), "")
	}
	if grown := memory() - before; grown > 64<<10 {
		t.Errorf("%d subscribers took %d bytes of PHP memory", listeners, grown)
	}

	start := time.Now()
	var wg sync.WaitGroup
	for range publishers {
		wg.Go(func() {
			resp, err := client.Post(base+"/publish?wait=1", "text/plain", strings.NewReader("tick"))
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		})
	}
	wg.Wait()
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("%d handlers that wait 1 s and publish took %s on one PHP thread", publishers, took)
	}
	for i, s := range subs {
		for n := range publishers {
			if ev := s.event(); ev["data"] != "tick" {
				t.Fatalf("subscriber %d, event %d: %v", i, n, ev)
			}
		}
	}
	// The subscribers were there before the requests' time limit and are
	// there after it.
	if wait := 3500*time.Millisecond - time.Since(start); wait > 0 {
		<-time.After(wait)
	}
	last := publish(t, base, "", "still here")
	if ev := subs[0].event(); ev["id"] != last {
		t.Errorf("after the time limit of requests: %v", ev)
	}
}

// With history(), a client that comes back with the id of the last update
// it saw gets what it missed, also from a server that was restarted.
func TestMercureHistory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "updates.db")
	base, stop := runMercure(t, "MERCURE_HISTORY="+file)
	first := publish(t, base, "", "one")
	second := publish(t, base, "", "two")
	back := subscribe(t, base, match(newsTopic)+"&last_event_id="+url.QueryEscape(first), "")
	if ev := back.event(); ev["id"] != second || ev["data"] != "two" {
		t.Errorf("after the first update: %v, want the second", ev)
	}
	back.resp.Body.Close()
	stop()

	base, _ = runMercure(t, "MERCURE_HISTORY="+file)
	all := subscribe(t, base, match(newsTopic)+"&last_event_id=earliest", "")
	if one, two := all.event(), all.event(); one["id"] != first || two["id"] != second {
		t.Errorf("from the earliest, after a restart: %v, %v", one, two)
	}
}

// compatibility(8) also takes the clients and the tokens of the protocol
// before 1.0, which the hub refuses without it.
func TestMercureCompatibility(t *testing.T) {
	old := mercureToken(t, map[string]any{"typ": "JWT"}, map[string]any{"mercure": map[string]any{"publish": []string{"*"}}})
	post := func(base string) int {
		form := url.Values{"topic": {newsTopic}, "data": {"from an old publisher"}}
		req, _ := http.NewRequest("POST", base+"/.well-known/mercure", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+old)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	strict, _ := runMercure(t)
	if status := post(strict); status != 401 {
		t.Errorf("a 0.x token without compatibility(): status %d, want 401", status)
	}

	base, _ := runMercure(t, "MERCURE_COMPATIBILITY=8")
	s := subscribe(t, base, "topic="+url.QueryEscape("https://example.com/{section}"), "")
	if status := post(base); status != 200 {
		t.Fatalf("a 0.x token with compatibility(8): status %d", status)
	}
	if ev := s.event(); ev["data"] != "from an old publisher" {
		t.Errorf("a 0.x subscriber got %v", ev)
	}
}

// A server that stops ends its subscribers itself: it does not wait for
// them until its grace time is over, and a client that was connected is no
// error in its log.
func TestMercureStopsWithTheServer(t *testing.T) {
	base, stop := runMercure(t, "MERCURE_SUBSCRIPTIONS=1")
	s := subscribe(t, base, match(newsTopic), "")
	publish(t, base, "", "before")
	s.event()
	start := time.Now()
	log := stop()
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the server took %s to stop with a subscriber", took)
	}
	// The hub logs no subscriber that comes or goes, and a client that was
	// connected when the server stopped is no error.
	for _, unwanted := range []string{"level=ERROR", "level=WARN", "ubscriber"} {
		if strings.Contains(log, unwanted) {
			t.Errorf("the log of a server that stopped with a subscriber has %q:\n%s", unwanted, log)
		}
	}
	if _, err := io.ReadAll(s.body); err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Errorf("the subscription of a stopped server: %v", err)
	}
}

// In isolated mode, where a request has its PHP thread to itself, the hub
// is the same: Go serves the subscribers and PHP publishes.
func TestMercureInIsolatedMode(t *testing.T) {
	base, _ := runMercure(t, "CAIRO_CONCURRENCY=isolated")
	s := subscribe(t, base, match(newsTopic), "")
	if id := publish(t, base, "", "isolated"); s.event()["id"] != id {
		t.Error("the subscriber did not get the update")
	}
}

// What cannot work is refused when the server starts, and a script has no
// hub to publish to.
func TestMercureDeclarationAndScripts(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct{ declare, want string }{
		"nobody could subscribe": {`$http->mercure();`, "issuer() or anonymous()"},
		"a route of the hub's":   {`$http->mercure()->anonymous(); $http->get('/.well-known/mercure/mine', fn () => Cairo\Http\Response::text('x'));`, "is the Mercure hub's"},
		"tokens for an unknown":  {`$http->mercure()->issuer('https://example.com', subscriber: str_repeat('k', 32));`, "resource() or publicUrls()"},
		"two hubs":               {`$http->mercure()->anonymous(); $app->http('localhost:0')->mercure()->anonymous();`, "one hub"},
	} {
		script := filepath.Join(dir, "server.php")
		code := "<?php\n$app = new Cairo\\App();\n$http = $app->http('127.0.0.1:0');\n" + c.declare + "\nreturn $app;\n"
		if err := os.WriteFile(script, []byte(code), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		out, err := exec.CommandContext(ctx, cairoBin, "serve", script, "--threads", "1", "--offload-threads", "1").CombinedOutput()
		cancel()
		if err == nil || !bytes.Contains(out, []byte(c.want)) {
			t.Errorf("%s: %v, output %s, want %q", name, err, out, c.want)
		}
	}

	script := filepath.Join(dir, "publish.php")
	code := `<?php try { mercure_publish('https://example.com/news', 'x'); } catch (RuntimeException $e) { echo 'refused: ', $e->getMessage(); }`
	if err := os.WriteFile(script, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(cairoBin, "php", script).CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "refused: no Mercure hub runs in this process") {
		t.Errorf("mercure_publish() in a script: %v: %s", err, out)
	}
}
