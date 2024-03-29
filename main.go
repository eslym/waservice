package main

import (
	"context"
	"flag"
	"fmt"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "github.com/glebarez/sqlite"
)

var readyState = struct {
	ready  bool
	lock   sync.RWMutex
	qrCode string
}{}

var (
	httpServe string
	serverKey string
	dbPath    string
)

func main() {
	flag.StringVar(&httpServe, "http", ":8080", "HTTP server listen address")
	flag.StringVar(&serverKey, "key", "", "HTTP server key")
	flag.StringVar(&dbPath, "db", "messages.db", "Database path")

	flag.Parse()

	dbLog := waLog.Stdout("Database", "INFO", true)

	// Make sure you add appropriate DB connector imports, e.g. github.com/mattn/go-sqlite3 for SQLite
	container, err := sqlstore.New("sqlite", fmt.Sprintf("file:%s?_pragma=foreign_keys(1)", dbPath), dbLog)
	if err != nil {
		panic(err)
	}
	// If you want multiple sessions, remember their JIDs and use .GetDevice(jid) or .GetAllDevices() instead.
	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		panic(err)
	}
	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	server := &http.Server{
		Addr: httpServe,
	}

	onClose := make(chan bool)

	go startHttpServer(server, client, onClose)

	var handler func(evt interface{})
	handler = func(evt interface{}) {
		switch v := evt.(type) {
		case *events.StreamError:
			_ = server.Close()
		case *events.QR:
			readyState.lock.Lock()
			readyState.qrCode = v.Codes[0]
			readyState.lock.Unlock()
		case *events.PairSuccess:
			readyState.lock.Lock()
			readyState.ready = true
			readyState.lock.Unlock()
		case *events.LoggedOut:
			readyState.lock.Lock()
			readyState.ready = false
			readyState.qrCode = ""
			readyState.lock.Unlock()
			go func() {
				time.Sleep(5 * time.Second)
				client = whatsmeow.NewClient(deviceStore, clientLog)
				client.AddEventHandler(handler)
				err = client.Connect()
				if err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "Error reconnecting: %s\n", err)
					_ = server.Shutdown(context.Background())
				}
			}()
		}
	}

	client.AddEventHandler(handler)

	err = client.Connect()
	if err != nil {
		panic(err)
	}

	if client.Store.ID != nil {
		readyState.lock.Lock()
		readyState.ready = true
		readyState.lock.Unlock()
	}

	// Listen to Ctrl+C (you can also do something else that prevents the program from exiting)
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	select {
	case <-c:
	case <-onClose:
	}

	client.Disconnect()
	_ = server.Shutdown(context.Background())

	time.Sleep(1 * time.Second)
}

func startHttpServer(server *http.Server, wa *whatsmeow.Client, onClose chan<- bool) {
	router := http.NewServeMux()
	router.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		readyState.lock.RLock()
		ready := readyState.ready
		readyState.lock.RUnlock()
		if ready {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
		}
	})
	router.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		readyState.lock.RLock()
		ready := readyState.ready
		readyState.lock.RUnlock()
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = r.ParseForm()
		key := r.Form.Get("key")
		if !safeEql(key, serverKey) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("403 Forbidden"))
			return
		}
		to := r.Form.Get("to")
		if to == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("to is required"))
			return
		}
		jid, err := types.ParseJID(to)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		text := r.Form.Get("text")
		if text == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("text is required"))
			return
		}
		msg := &proto.Message{
			Conversation: &text,
		}
		_, err = wa.SendMessage(context.Background(), jid, msg)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	router.HandleFunc("/qr", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if !safeEql(key, serverKey) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("403 Forbidden"))
			return
		}
		readyState.lock.RLock()
		qrCode := readyState.qrCode
		ready := readyState.ready
		readyState.lock.RUnlock()
		if ready {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("already logged in"))
			return
		}
		if qrCode == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("no QR code available"))
			return
		}
		w.Header().Set("Content-Type", "image/png")
		png, err := qrcode.Encode(qrCode, qrcode.Medium, 256)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(png)))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	})
	server.Handler = router
	err := server.ListenAndServe()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error starting HTTP server: %s\n", err)
	}
	onClose <- true
}

func safeEql(a string, b string) bool {
	if len(a) != len(b) {
		return false
	}
	ret := true
	for i := 0; i < len(a); i++ {
		ret = ret && a[i] == b[i]
	}
	return ret
}
