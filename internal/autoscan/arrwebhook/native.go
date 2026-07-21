package arrwebhook

import (
	"encoding/json"
	"strings"

	"github.com/Silo-Server/silo-server/internal/autoscan"
)

// ProviderNative is a source that speaks autoscan's own vocabulary instead of
// imitating an arr.
//
// A scan_source plugin that WRITES files knows the instant each one lands, so
// making the host poll it is strictly worse than letting it say so: the poll
// interval is the floor on how long a new file stays invisible, and for
// on-demand libraries that delay is the whole user-visible latency. Webhook
// delivery already exists for exactly this shape of problem — it was just
// reachable only by payloads pretending to be Sonarr or Radarr.
//
// Rather than have plugins fake a movieFile block and depend on this package's
// shape inference, they can post the paths directly.
const ProviderNative = "native"

// nativePayload is the native delivery shape.
//
// Deliberately close to autoscan.Change so there is nothing to infer: a sender
// states its paths and what each one is, and this package does no guessing.
type nativePayload struct {
	EventType string `json:"eventType"`
	Changes   []struct {
		SourcePath string `json:"source_path"`
		Scope      string `json:"scope"`
	} `json:"changes"`
}

// parseNative classifies a native delivery.
//
// Unlike the arr parsers this does not switch on event type: a native sender
// already decided what it is reporting, and the paths mean the same thing
// whether the file was added, replaced, or renamed. The event type is carried
// through for the admin UI only.
func parseNative(body []byte) (ParsedWebhook, error) {
	var p nativePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return ParsedWebhook{}, ErrMalformedPayload
	}
	eventType := strings.TrimSpace(p.EventType)
	if eventType == "" {
		return ParsedWebhook{}, ErrMalformedPayload
	}

	out := ParsedWebhook{Provider: ProviderNative, EventType: eventType}
	if strings.EqualFold(eventType, "Test") {
		out.Test = true
		return out, nil
	}

	seen := make(map[string]struct{}, len(p.Changes))
	for _, c := range p.Changes {
		path := strings.TrimSpace(c.SourcePath)
		if path == "" {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		out.Changes = append(out.Changes, autoscan.Change{
			SourcePath: path,
			Scope:      nativeScope(c.Scope),
		})
	}
	return out, nil
}

// nativeScope maps the wire value to a change scope.
//
// Anything unrecognised — including an omitted scope — becomes a subtree scan.
// That is the conservative direction: a subtree scan over-scans slightly, while
// wrongly claiming file scope on a directory would silently miss everything
// inside it.
func nativeScope(scope string) autoscan.ChangeScope {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "file":
		return autoscan.ChangeScopeFile
	default:
		return autoscan.ChangeScopeSubtree
	}
}
