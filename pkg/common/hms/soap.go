/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hms

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Wire defaults for hms.version.version29. The VMODL service namespace is
// "hmssrv" and its version ID is "9.2.1"; vmomi turns these into the XML
// namespace "urn:hmssrv" and the SOAPAction "urn:hmssrv/9.2.1".
const (
	DefaultNamespace = "urn:hmssrv"
	DefaultVersion   = "9.2.1"
	// DefaultPath is where the DPVA proxies the HMS VMOMI endpoint.
	DefaultPath = "/vrms"

	soapEnvNS = "http://schemas.xmlsoap.org/soap/envelope/"
	xsiNS     = "http://www.w3.org/2001/XMLSchema-instance"
	xsdNS     = "http://www.w3.org/2001/XMLSchema"
	wsseNS    = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"
	wsuNS     = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"
)

// Managed object IDs of the HMS singletons (HmsMoIds).
var (
	sessionManager     = MoRef{Type: "HmsSessionManager", Value: "session-manager"}
	siteManager        = MoRef{Type: "HmsRemoteSiteManager", Value: "site-manager"}
	replicationManager = MoRef{Type: "HmsReplicationManager", Value: "replication-manager"}
	replicaManager     = MoRef{Type: "HmsReplicaManager", Value: "replica-manager"}
)

// MarshalXML writes the reference as <name type="T">value</name>.
func (r MoRef) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "type"}, Value: r.Type})
	return e.EncodeElement(r.Value, start)
}

// UnmarshalXML reads the unqualified "type" attribute and ignores xsi:type,
// which vmomi sets on references returned as xsd:anyType.
func (r *MoRef) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for _, a := range start.Attr {
		if a.Name.Local == "type" && a.Name.Space == "" {
			r.Type = a.Value
		}
	}
	var v string
	if err := d.DecodeElement(&v, &start); err != nil {
		return err
	}
	r.Value = strings.TrimSpace(v)
	return nil
}

// Fault is a SOAP fault returned by HMS.
type Fault struct {
	// Type is the vmomi fault type, for example "HmsAlreadyRecoveredFault".
	Type    string
	Message string
}

func (f *Fault) Error() string {
	if f.Type == "" {
		return f.Message
	}
	return fmt.Sprintf("%s: %s", f.Type, f.Message)
}

// Is maps HMS fault types to the package errors.
func (f *Fault) Is(target error) bool {
	t := f.Type
	has := func(names ...string) bool {
		for _, n := range names {
			if strings.Contains(t, n) {
				return true
			}
		}
		return false
	}
	switch target {
	case ErrNotFound:
		return has("NotFound", "GroupNotConfigured")
	case ErrNotSupported:
		// An HMS build without a method rejects it as an invalid request.
		return has("NotSupported", "NotImplemented") ||
			(has("InvalidRequest") && strings.Contains(f.Message, "Invalid managed method"))
	case ErrAlreadyRecovered:
		return has("AlreadyRecovered", "GroupCommittedToImage")
	case ErrSyncOngoing:
		return has("SyncOngoing")
	case ErrNotAuthenticated:
		return has("NotAuthenticated", "SecurityError")
	case ErrPeerUnreachable:
		return has("ConnectionFault", "HmsServerNotRegistered", "NoPeerSession", "PeerNotConnected",
			"CannotVerifyCredentials")
	case ErrInvalidState:
		return has("OngoingRecovery", "OngoingTest", "InvalidState", "InvalidVmPowerState")
	}
	return false
}

// soapTransport sends vmomi SOAP requests to one HMS endpoint. The HTTP
// client's cookie jar carries the session cookie.
type soapTransport struct {
	httpClient *http.Client
	url        string
	namespace  string
	version    string
}

// call invokes a method. req must marshal to the method element, without a
// namespace; res is decoded from the response element and may be nil.
// security is an optional WS-Security header.
func (t *soapTransport) call(ctx context.Context, req any, res any, security string) error {
	body, err := xml.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	// Put the method element in the service namespace; its children inherit it.
	body = setNamespace(body, t.namespace)

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	fmt.Fprintf(&buf, `<soapenv:Envelope xmlns:soapenv=%q xmlns:xsd=%q xmlns:xsi=%q>`, soapEnvNS, xsdNS, xsiNS)
	if security != "" {
		buf.WriteString("<soapenv:Header>")
		buf.WriteString(security)
		buf.WriteString("</soapenv:Header>")
	}
	buf.WriteString("<soapenv:Body>")
	buf.Write(body)
	buf.WriteString("</soapenv:Body></soapenv:Envelope>")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, &buf)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	httpReq.Header.Set("SOAPAction", fmt.Sprintf("%q", t.namespace+"/"+t.version))
	httpRes, err := t.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpRes.Body.Close()
	data, err := io.ReadAll(httpRes.Body)
	if err != nil {
		return err
	}

	var env struct {
		Body struct {
			Fault *struct {
				String string `xml:"faultstring"`
				Detail struct {
					Items []struct {
						XMLName xml.Name
						Type    string `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
					} `xml:",any"`
				} `xml:"detail"`
			} `xml:"Fault"`
			Inner []byte `xml:",innerxml"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("HTTP %d: decode response: %w", httpRes.StatusCode, err)
	}
	if f := env.Body.Fault; f != nil {
		fault := &Fault{Message: strings.TrimSpace(f.String)}
		if len(f.Detail.Items) > 0 {
			item := f.Detail.Items[0]
			fault.Type = item.Type
			if fault.Type == "" {
				fault.Type = strings.TrimSuffix(item.XMLName.Local, "Fault")
			}
			if i := strings.IndexByte(fault.Type, ':'); i >= 0 {
				fault.Type = fault.Type[i+1:]
			}
		}
		return fault
	}
	if httpRes.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", httpRes.StatusCode, truncate(string(data), 200))
	}
	if res == nil {
		return nil
	}
	// The body is decoded on its own, so carry over the envelope's xsi prefix.
	inner := bytes.TrimSpace(env.Body.Inner)
	if end := bytes.IndexByte(inner, '>'); end < 0 || !bytes.Contains(inner[:end], []byte("xmlns:xsi=")) {
		inner = setAttr(inner, fmt.Sprintf("xmlns:xsi=%q", xsiNS))
	}
	if err := xml.Unmarshal(inner, res); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// setNamespace adds xmlns to the first element of an encoded request.
func setNamespace(body []byte, ns string) []byte {
	return setAttr(body, fmt.Sprintf("xmlns=%q", ns))
}

// setAttr adds an attribute to the first element of an XML fragment.
func setAttr(body []byte, attr string) []byte {
	i := bytes.IndexAny(body, " />")
	if i < 0 {
		return body
	}
	out := make([]byte, 0, len(body)+len(attr)+1)
	out = append(out, body[:i]...)
	out = append(out, ' ')
	out = append(out, attr...)
	return append(out, body[i:]...)
}

// bearerSecurityHeader wraps a SAML bearer assertion in a WS-Security header.
func bearerSecurityHeader(assertion string, created, expires string) string {
	return fmt.Sprintf(`<wsse:Security xmlns:wsse=%q xmlns:wsu=%q>`+
		`<wsu:Timestamp><wsu:Created>%s</wsu:Created><wsu:Expires>%s</wsu:Expires></wsu:Timestamp>`+
		`%s</wsse:Security>`, wsseNS, wsuNS, created, expires, assertion)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
