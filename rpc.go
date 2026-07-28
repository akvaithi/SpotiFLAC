package main

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
)

// handleRPC exposes every exported method of *App over HTTP, mirroring the way
// Wails bound the same methods to the desktop frontend.
//
//	POST /api/rpc/{MethodName}
//	body: JSON array of positional arguments, e.g.
//	      DownloadTrack       -> [ { ...DownloadRequest... } ]
//	      GetStreamingURLs    -> [ "trackId", "US" ]
//	      GetDownloadProgress -> []   (or empty body)
//
// Response: the method's non-error return value as JSON. If the method's last
// return value is a non-nil error, responds 500 with {"error": "..."}.
func handleRPC(app *App) http.HandlerFunc {
	appVal := reflect.ValueOf(app)
	errType := reflect.TypeOf((*error)(nil)).Elem()

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "use POST")
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/rpc/")
		if name == "" || strings.ContainsAny(name, "/") {
			writeErr(w, http.StatusBadRequest, "invalid method name")
			return
		}

		method := appVal.MethodByName(name)
		if !method.IsValid() {
			writeErr(w, http.StatusNotFound, "unknown method: "+name)
			return
		}
		mType := method.Type()

		// Parse positional arguments.
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024*1024))
		var rawArgs []json.RawMessage
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := json.Unmarshal(body, &rawArgs); err != nil {
				writeErr(w, http.StatusBadRequest, "arguments must be a JSON array: "+err.Error())
				return
			}
		}

		if len(rawArgs) != mType.NumIn() {
			writeErr(w, http.StatusBadRequest,
				name+" expects "+itoa(mType.NumIn())+" argument(s), got "+itoa(len(rawArgs)))
			return
		}

		args := make([]reflect.Value, mType.NumIn())
		for i := 0; i < mType.NumIn(); i++ {
			ptr := reflect.New(mType.In(i))
			if err := json.Unmarshal(rawArgs[i], ptr.Interface()); err != nil {
				writeErr(w, http.StatusBadRequest,
					"argument "+itoa(i)+" for "+name+": "+err.Error())
				return
			}
			args[i] = ptr.Elem()
		}

		results := method.Call(args)

		// Separate a trailing error return, if any.
		var errVal error
		var payloads []interface{}
		for _, res := range results {
			if res.Type().Implements(errType) {
				if !res.IsNil() {
					errVal = res.Interface().(error)
				}
				continue
			}
			payloads = append(payloads, res.Interface())
		}

		if errVal != nil {
			writeErr(w, http.StatusInternalServerError, errVal.Error())
			return
		}

		switch len(payloads) {
		case 0:
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		case 1:
			writeJSON(w, http.StatusOK, payloads[0])
		default:
			writeJSON(w, http.StatusOK, payloads)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
