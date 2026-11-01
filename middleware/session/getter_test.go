package session

import (
	"encoding/gob"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/storage/memory"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func Test_GetAs_Middleware(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New())
	app.Get("/set", func(c fiber.Ctx) error {
		FromContext(c).Set("n", 42)
		return nil
	})
	app.Get("/get", func(c fiber.Ctx) error {
		n, ok := GetAs[int](FromContext(c), "n")
		s, sok := GetAs[string](FromContext(c), "n")
		return c.SendString(strconv.Itoa(n) + strconv.FormatBool(ok) + "|" + s + strconv.FormatBool(sok))
	})

	resp, _ := sendRequest(t, app, "/set", nil)
	id := cookiesByName(resp)["session_id"].Value
	_, body := sendRequest(t, app, "/get", map[string]string{"session_id": id})
	require.Equal(t, "42true|false", body)

	t.Run("without the middleware, FromContext is nil and GetAs says so", func(t *testing.T) {
		t.Parallel()
		bare := fiber.New()
		bare.Get("/", func(c fiber.Ctx) error {
			n, ok := GetAs[int](FromContext(c), "n")
			return c.SendString(strconv.Itoa(n) + strconv.FormatBool(ok))
		})
		_, body := sendRequest(t, bare, "/", nil)
		require.Equal(t, "0false", body)
	})
}

// testing.AllocsPerRun panics in a parallel test, so this one stays sequential.
func Test_GetAs_Allocations(t *testing.T) {
	sess := newSessionWithStorage(t, memory.New())
	sess.Set("name", "alice")
	sess.Set("count", 3)

	require.Zero(t, testing.AllocsPerRun(100, func() { _, _ = GetAs[string](sess, "name") }))
	require.Zero(t, testing.AllocsPerRun(100, func() { _, _ = GetAs[int](sess, "count") }))
	require.Zero(t, testing.AllocsPerRun(100, func() { _, _ = GetAs[string](sess, "missing") }))
	require.Zero(t, testing.AllocsPerRun(100, func() { _, _ = GetAs[string]((*Session)(nil), "name") }))

	// A value that is costly to copy is no reason to allocate: the variable that a codec
	// would decode into is made only when there is one.
	type big struct {
		Name  string
		Bytes [1024]byte
	}
	sess.Set("big", big{Name: "big"})
	sess.Set("pointer", &big{Name: "pointer"})
	require.Zero(t, testing.AllocsPerRun(100, func() { _, _ = GetAs[big](sess, "big") }))
	require.Zero(t, testing.AllocsPerRun(100, func() { _, _ = GetAs[*big](sess, "pointer") }))
	require.Zero(t, testing.AllocsPerRun(100, func() { _, _ = GetAs[big](sess, "missing") }))
}

func Test_Session_IdleTimeout(t *testing.T) {
	t.Parallel()

	t.Run("the store's until one is set", func(t *testing.T) {
		t.Parallel()
		sess := newSessionWithStorage(t, memory.New())
		require.Equal(t, 30*time.Minute, sess.IdleTimeout())

		sess.SetIdleTimeout(5 * time.Minute)
		require.Equal(t, 5*time.Minute, sess.IdleTimeout())
	})

	t.Run("a store with another idle timeout", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		store := NewStore(Config{IdleTimeout: 2 * time.Hour})
		id := seedSession(t, app, store, func(s *Session) { s.Set("k", "v") })
		require.Equal(t, 2*time.Hour, loadSession(t, app, store, id).IdleTimeout())
	})

	t.Run("a timeout that was set is not kept for the next request", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		store := NewStore(Config{IdleTimeout: time.Hour})
		id := seedSession(t, app, store, func(s *Session) {
			s.Set("k", "v")
			s.SetIdleTimeout(24 * time.Hour)
		})
		require.Equal(t, time.Hour, loadSession(t, app, store, id).IdleTimeout())
	})

	t.Run("a session without a store", func(t *testing.T) {
		t.Parallel()
		require.Zero(t, (&Session{}).IdleTimeout())
	})

	t.Run("through the middleware", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		app.Use(New(Config{IdleTimeout: 10 * time.Minute}))
		app.Get("/", func(c fiber.Ctx) error {
			m := FromContext(c)
			before := m.IdleTimeout()
			m.Session.SetIdleTimeout(time.Minute)
			return c.SendString(before.String() + "|" + m.IdleTimeout().String())
		})
		_, body := sendRequest(t, app, "/", nil)
		require.Equal(t, "10m0s|1m0s", body)
	})
}

func Test_Session_AbsoluteExpiry(t *testing.T) {
	t.Parallel()

	t.Run("none without an absolute timeout", func(t *testing.T) {
		t.Parallel()
		sess := newSessionWithStorage(t, memory.New())
		expiry, ok := sess.AbsoluteExpiry()
		require.False(t, ok)
		require.True(t, expiry.IsZero())
	})

	t.Run("a new session ends one absolute timeout from now", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		store := NewStore(Config{AbsoluteTimeout: time.Hour})
		id := seedSession(t, app, store, func(s *Session) { s.Set("k", "v") })

		sess := loadSession(t, app, store, id)
		expiry, ok := sess.AbsoluteExpiry()
		require.True(t, ok)
		require.WithinDuration(t, time.Now().Add(time.Hour), expiry, 10*time.Second)
	})

	t.Run("it survives storage and regeneration, and reset starts it over", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		store := NewStore(Config{AbsoluteTimeout: time.Hour})
		id := seedSession(t, app, store, func(s *Session) { s.Set("k", "v") })

		sess := loadSession(t, app, store, id)
		original, ok := sess.AbsoluteExpiry()
		require.True(t, ok)

		require.NoError(t, sess.Regenerate())
		kept, ok := sess.AbsoluteExpiry()
		require.True(t, ok)
		require.True(t, original.Equal(kept), "a new ID does not buy a longer session")

		time.Sleep(10 * time.Millisecond)
		require.NoError(t, sess.Reset())
		restarted, ok := sess.AbsoluteExpiry()
		require.True(t, ok)
		require.True(t, restarted.After(original), "a reset is a new session with a new deadline")
	})

	t.Run("through the middleware", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		app.Use(New(Config{AbsoluteTimeout: time.Hour}))
		app.Get("/", func(c fiber.Ctx) error {
			expiry, ok := FromContext(c).AbsoluteExpiry()
			return c.SendString(strconv.FormatBool(ok) + "|" + strconv.FormatBool(time.Until(expiry) > 59*time.Minute))
		})
		_, body := sendRequest(t, app, "/", nil)
		require.Equal(t, "true|true", body)
	})
}

// Benchmark_GetAs compares the typed read with the type assertion it replaces.
//
// go test -v -run=^$ -bench=Benchmark_GetAs -benchmem -count=4
func Benchmark_GetAs(b *testing.B) {
	app := fiber.New()
	c := app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(c)

	sess, err := NewStore().Get(c)
	if err != nil {
		b.Fatal(err)
	}
	defer sess.Release()
	sess.Set("name", "alice")

	b.Run("GetAs", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, ok := GetAs[string](sess, "name"); !ok {
				b.Fatal("not found")
			}
		}
	})

	b.Run("Get and assert", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, ok := sess.Get("name").(string); !ok {
				b.Fatal("not found")
			}
		}
	})
}

func init() {
	gob.Register(map[string]int{})
}

// A session without its data map answers like an empty one.
func Test_Session_WithoutDataAnswersLikeAnEmptyOne(t *testing.T) {
	t.Parallel()

	var sess Session
	require.NotPanics(t, func() {
		expiry, ok := sess.AbsoluteExpiry()
		require.False(t, ok)
		require.True(t, expiry.IsZero())

		value, found := GetAs[string](&sess, "k")
		require.False(t, found)
		require.Empty(t, value)

		var target string
		require.ErrorIs(t, sess.GetInto("k", &target), ErrKeyNotFound)
	})
}

// The target of Middleware.GetInto is checked as that of Session.GetInto is.
func Test_Middleware_GetInto_TargetHasToBeANonNilPointer(t *testing.T) {
	t.Parallel()

	for name, codec := range map[string]Codec{"gob": nil, "json": JSONCodec{}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(New(Config{Codec: codec}))
			app.Get("/", func(c fiber.Ctx) error {
				m := FromContext(c)
				m.Set("k", 1)

				var nilPointer *int
				require.NotPanics(t, func() {
					require.Error(t, m.GetInto("k", nil))
					require.Error(t, m.GetInto("k", 5))
					require.Error(t, m.GetInto("k", nilPointer))
					require.Error(t, m.GetInto("missing", nil), "the target is checked before the key is looked up")
				})
				var n int
				require.NoError(t, m.GetInto("k", &n))
				require.Equal(t, 1, n)
				return nil
			})
			require.Equal(t, fiber.StatusOK, serveGet(app, "/"))
		})
	}
}

// serveGet runs a GET request through the app and returns the status code.
func serveGet(app *fiber.App, uri string) int {
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodGet)
	fctx.Request.SetRequestURI(uri)
	app.Handler()(fctx)
	return fctx.Response.StatusCode()
}
