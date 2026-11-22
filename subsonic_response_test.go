package main

import (
	"encoding/json"
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAcquisitionResponsesHonorClientFormat(t *testing.T) {
	for _, format := range []string{"", "xml", "json"} {
		for _, failed := range []bool{false, true} {
			request := httptest.NewRequest("GET", "/rest/star.view?f="+format, nil)
			response := httptest.NewRecorder()
			if failed {
				writeSubsonicError(response, request, 70, "Queued & waiting")
			} else {
				writeSubsonicOK(response, request)
			}
			want := "ok"
			if failed {
				want = "failed"
			}
			if format == "json" {
				var envelope subsonicEnvelope
				if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Response.Status != want {
					t.Fatalf("JSON response: %v", err)
				}
			} else {
				var envelope struct {
					Status string `xml:"status,attr"`
					Error  *struct {
						Code    int    `xml:"code,attr"`
						Message string `xml:"message,attr"`
					} `xml:"error"`
				}
				if err := xml.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Status != want {
					t.Fatalf("XML response: %v", err)
				}
				if !strings.Contains(response.Header().Get("Content-Type"), "xml") {
					t.Fatal("missing XML content type")
				}
				if failed && (envelope.Error == nil || envelope.Error.Code != 70 || envelope.Error.Message != "Queued & waiting") {
					t.Fatal("error did not round-trip through XML escaping")
				}
			}
		}
	}
}
