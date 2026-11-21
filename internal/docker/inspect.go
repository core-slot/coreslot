//go:build unix

package docker

import (
	"encoding/json"
	"time"
)

// Inspect resolves a container reference, a name or a short id, to the container's id as the engine
// does (GET /containers/<ref>/json). The caller has validated ref: it goes into the request as it is.
// An answer whose Id is not one the engine makes (IsID) is no answer: code Parse.
func Inspect(sock, ref string, deadline time.Time) (id string, code string) {
	body, _, code := Get(sock, "/containers/"+ref+"/json", deadline, ResponseMax)
	if code != "" {
		return "", code
	}
	var in struct{ Id string }
	if json.Unmarshal(body, &in) != nil || !IsID(in.Id) {
		return "", Parse
	}
	return in.Id, ""
}
