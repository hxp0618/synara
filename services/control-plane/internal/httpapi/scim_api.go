package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/scim"
)

func (s *Server) scimServiceProviderConfig(w http.ResponseWriter, _ *http.Request) {
	writeSCIM(w, http.StatusOK, map[string]any{
		"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"patch":   map[string]any{"supported": true}, "bulk": map[string]any{"supported": false},
		"filter":         map[string]any{"supported": false, "maxResults": 200},
		"changePassword": map[string]any{"supported": false}, "sort": map[string]any{"supported": false},
		"etag": map[string]any{"supported": false},
	})
}

func (s *Server) scimResourceTypes(w http.ResponseWriter, _ *http.Request) {
	writeSCIM(w, http.StatusOK, map[string]any{
		"schemas":      []string{"urn:ietf:params:scim:api:messages:2.0:ListResponse"},
		"totalResults": 2, "startIndex": 1, "itemsPerPage": 2,
		"Resources": []map[string]any{
			{"id": "User", "name": "User", "endpoint": "/Users", "schema": "urn:ietf:params:scim:schemas:core:2.0:User"},
			{"id": "Group", "name": "Group", "endpoint": "/Groups", "schema": "urn:ietf:params:scim:schemas:core:2.0:Group"},
		},
	})
}

func (s *Server) scimSchemas(w http.ResponseWriter, _ *http.Request) {
	writeSCIM(w, http.StatusOK, map[string]any{
		"schemas":      []string{"urn:ietf:params:scim:api:messages:2.0:ListResponse"},
		"totalResults": 2, "startIndex": 1, "itemsPerPage": 2,
		"Resources": []map[string]any{
			{"id": "urn:ietf:params:scim:schemas:core:2.0:User", "name": "User"},
			{"id": "urn:ietf:params:scim:schemas:core:2.0:Group", "name": "Group"},
		},
	})
}

func (s *Server) scimListUsers(w http.ResponseWriter, r *http.Request) {
	startIndex, count, ok := scimPage(w, r)
	if !ok {
		return
	}
	result, err := s.scim.ListUsers(r.Context(), mustServiceAccount(r), startIndex, count)
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	writeSCIM(w, http.StatusOK, result)
}

func (s *Server) scimGetUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	item, err := s.scim.GetUser(r.Context(), mustServiceAccount(r), userID)
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	writeSCIM(w, http.StatusOK, item)
}

func (s *Server) scimCreateUser(w http.ResponseWriter, r *http.Request) {
	var input scim.UserInput
	if err := decodeSCIM(r, &input); err != nil {
		writeSCIMError(w, err)
		return
	}
	item, err := s.scim.CreateUser(r.Context(), mustServiceAccount(r), input, requestID(r), clientIP(r))
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	writeSCIM(w, http.StatusCreated, item)
}

func (s *Server) scimReplaceUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	var input scim.UserInput
	if err := decodeSCIM(r, &input); err != nil {
		writeSCIMError(w, err)
		return
	}
	item, err := s.scim.ReplaceUser(r.Context(), mustServiceAccount(r), userID, input, requestID(r), clientIP(r))
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	writeSCIM(w, http.StatusOK, item)
}

type scimPatchRequest struct {
	Schemas    []string             `json:"schemas"`
	Operations []scimPatchOperation `json:"Operations"`
}

type scimPatchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

func (s *Server) scimPatchUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	current, err := s.scim.GetUser(r.Context(), mustServiceAccount(r), userID)
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	active := current.Active
	input := scim.UserInput{ExternalID: current.ExternalID, UserName: current.UserName, DisplayName: current.DisplayName, Active: &active, Emails: current.Emails}
	var patch scimPatchRequest
	if err := decodeSCIM(r, &patch); err != nil {
		writeSCIMError(w, err)
		return
	}
	for _, operation := range patch.Operations {
		if err := applyUserPatch(&input, operation); err != nil {
			writeSCIMError(w, err)
			return
		}
	}
	item, err := s.scim.ReplaceUser(r.Context(), mustServiceAccount(r), userID, input, requestID(r), clientIP(r))
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	writeSCIM(w, http.StatusOK, item)
}

func (s *Server) scimDeleteUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	if err := s.scim.DeleteUser(r.Context(), mustServiceAccount(r), userID, requestID(r), clientIP(r)); err != nil {
		writeSCIMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) scimListGroups(w http.ResponseWriter, r *http.Request) {
	startIndex, count, ok := scimPage(w, r)
	if !ok {
		return
	}
	result, err := s.scim.ListGroups(r.Context(), mustServiceAccount(r), startIndex, count)
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	writeSCIM(w, http.StatusOK, result)
}

func (s *Server) scimGetGroup(w http.ResponseWriter, r *http.Request) {
	groupID, ok := s.pathUUID(w, r, "groupID")
	if !ok {
		return
	}
	item, err := s.scim.GetGroup(r.Context(), mustServiceAccount(r), groupID)
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	writeSCIM(w, http.StatusOK, item)
}

func (s *Server) scimCreateGroup(w http.ResponseWriter, r *http.Request) {
	var input scim.GroupInput
	if err := decodeSCIM(r, &input); err != nil {
		writeSCIMError(w, err)
		return
	}
	item, err := s.scim.CreateGroup(r.Context(), mustServiceAccount(r), input, requestID(r), clientIP(r))
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	writeSCIM(w, http.StatusCreated, item)
}

func (s *Server) scimReplaceGroup(w http.ResponseWriter, r *http.Request) {
	groupID, ok := s.pathUUID(w, r, "groupID")
	if !ok {
		return
	}
	var input scim.GroupInput
	if err := decodeSCIM(r, &input); err != nil {
		writeSCIMError(w, err)
		return
	}
	item, err := s.scim.ReplaceGroup(r.Context(), mustServiceAccount(r), groupID, input, requestID(r), clientIP(r))
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	writeSCIM(w, http.StatusOK, item)
}

func (s *Server) scimPatchGroup(w http.ResponseWriter, r *http.Request) {
	groupID, ok := s.pathUUID(w, r, "groupID")
	if !ok {
		return
	}
	current, err := s.scim.GetGroup(r.Context(), mustServiceAccount(r), groupID)
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	input := scim.GroupInput{ExternalID: current.ExternalID, DisplayName: current.DisplayName, Members: current.Members}
	var patch scimPatchRequest
	if err := decodeSCIM(r, &patch); err != nil {
		writeSCIMError(w, err)
		return
	}
	for _, operation := range patch.Operations {
		if err := applyGroupPatch(&input, operation); err != nil {
			writeSCIMError(w, err)
			return
		}
	}
	item, err := s.scim.ReplaceGroup(r.Context(), mustServiceAccount(r), groupID, input, requestID(r), clientIP(r))
	if err != nil {
		writeSCIMError(w, err)
		return
	}
	writeSCIM(w, http.StatusOK, item)
}

func (s *Server) scimDeleteGroup(w http.ResponseWriter, r *http.Request) {
	groupID, ok := s.pathUUID(w, r, "groupID")
	if !ok {
		return
	}
	if err := s.scim.DeleteGroup(r.Context(), mustServiceAccount(r), groupID, requestID(r), clientIP(r)); err != nil {
		writeSCIMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func scimPage(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	startIndex, err := strconv.Atoi(defaultString(r.URL.Query().Get("startIndex"), "1"))
	if err != nil {
		writeSCIMError(w, problem.New(400, "invalid_scim_pagination", "SCIM startIndex must be an integer."))
		return 0, 0, false
	}
	count, err := strconv.Atoi(defaultString(r.URL.Query().Get("count"), "100"))
	if err != nil {
		writeSCIMError(w, problem.New(400, "invalid_scim_pagination", "SCIM count must be an integer."))
		return 0, 0, false
	}
	return startIndex, count, true
}

func decodeSCIM(r *http.Request, target any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" && mediaType != "application/scim+json" {
		return problem.New(415, "unsupported_media_type", "Content-Type must be application/scim+json or application/json.")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxJSONBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return problem.Wrap(400, "invalid_scim_payload", "SCIM request body is not valid JSON.", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return problem.New(400, "invalid_scim_payload", "SCIM request body must contain one JSON value.")
	}
	return nil
}

func writeSCIM(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/scim+json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeSCIMError(w http.ResponseWriter, err error) {
	var apiError *problem.Error
	if !errors.As(err, &apiError) {
		apiError = problem.Wrap(500, "internal_error", "The SCIM operation failed.", err)
	}
	writeSCIM(w, apiError.Status, map[string]any{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:Error"},
		"status":  strconv.Itoa(apiError.Status), "detail": apiError.Message,
	})
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func applyUserPatch(input *scim.UserInput, operation scimPatchOperation) error {
	op := strings.ToLower(strings.TrimSpace(operation.Op))
	path := strings.ToLower(strings.TrimSpace(operation.Path))
	if op != "add" && op != "replace" && op != "remove" {
		return problem.New(400, "invalid_scim_patch", "SCIM patch operation is not supported.")
	}
	if path == "" {
		var value struct {
			ExternalID  *string `json:"externalId"`
			UserName    *string `json:"userName"`
			DisplayName *string `json:"displayName"`
			Active      *bool   `json:"active"`
		}
		if err := json.Unmarshal(operation.Value, &value); err != nil {
			return problem.New(400, "invalid_scim_patch", "SCIM User patch value is invalid.")
		}
		if value.ExternalID != nil {
			input.ExternalID = *value.ExternalID
		}
		if value.UserName != nil {
			input.UserName = *value.UserName
		}
		if value.DisplayName != nil {
			input.DisplayName = *value.DisplayName
		}
		if value.Active != nil {
			input.Active = value.Active
		}
		return nil
	}
	switch path {
	case "active":
		if op == "remove" {
			inactive := false
			input.Active = &inactive
			return nil
		}
		var value bool
		if err := json.Unmarshal(operation.Value, &value); err != nil {
			return problem.New(400, "invalid_scim_patch", "SCIM active patch value must be boolean.")
		}
		input.Active = &value
	case "displayname", "username", "externalid":
		value := ""
		if op != "remove" {
			if err := json.Unmarshal(operation.Value, &value); err != nil {
				return problem.New(400, "invalid_scim_patch", "SCIM User patch value must be a string.")
			}
		}
		switch path {
		case "displayname":
			input.DisplayName = value
		case "username":
			input.UserName = value
		case "externalid":
			input.ExternalID = value
		}
	default:
		return problem.New(400, "invalid_scim_patch", "SCIM User patch path is not supported.")
	}
	return nil
}

func applyGroupPatch(input *scim.GroupInput, operation scimPatchOperation) error {
	op := strings.ToLower(strings.TrimSpace(operation.Op))
	path := strings.TrimSpace(operation.Path)
	lowerPath := strings.ToLower(path)
	if op != "add" && op != "replace" && op != "remove" {
		return problem.New(400, "invalid_scim_patch", "SCIM patch operation is not supported.")
	}
	if lowerPath == "" {
		var value struct {
			ExternalID  *string        `json:"externalId"`
			DisplayName *string        `json:"displayName"`
			Members     *[]scim.Member `json:"members"`
		}
		if err := json.Unmarshal(operation.Value, &value); err != nil {
			return problem.New(400, "invalid_scim_patch", "SCIM Group patch value is invalid.")
		}
		if value.ExternalID != nil {
			input.ExternalID = *value.ExternalID
		}
		if value.DisplayName != nil {
			input.DisplayName = *value.DisplayName
		}
		if value.Members != nil {
			input.Members = append([]scim.Member(nil), (*value.Members)...)
		}
		return nil
	}
	switch {
	case lowerPath == "displayname" || lowerPath == "externalid":
		value := ""
		if op != "remove" {
			if err := json.Unmarshal(operation.Value, &value); err != nil {
				return problem.New(400, "invalid_scim_patch", "SCIM Group patch value must be a string.")
			}
		}
		if lowerPath == "displayname" {
			input.DisplayName = value
		} else {
			input.ExternalID = value
		}
	case lowerPath == "members":
		if op == "remove" {
			input.Members = nil
			return nil
		}
		members, err := decodePatchMembers(operation.Value)
		if err != nil {
			return err
		}
		if op == "replace" {
			input.Members = members
		} else {
			input.Members = append(input.Members, members...)
		}
	case strings.HasPrefix(lowerPath, "members[") && op == "remove":
		memberID := filteredMemberID(path)
		if memberID == "" {
			return problem.New(400, "invalid_scim_patch", "SCIM Group member filter is invalid.")
		}
		kept := input.Members[:0]
		for _, member := range input.Members {
			if member.Value != memberID {
				kept = append(kept, member)
			}
		}
		input.Members = kept
	default:
		return problem.New(400, "invalid_scim_patch", "SCIM Group patch path is not supported.")
	}
	return nil
}

func decodePatchMembers(raw json.RawMessage) ([]scim.Member, error) {
	var members []scim.Member
	if err := json.Unmarshal(raw, &members); err == nil {
		return members, nil
	}
	var wrapped struct {
		Members []scim.Member `json:"members"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, problem.New(400, "invalid_scim_patch", "SCIM Group members patch value is invalid.")
	}
	return wrapped.Members, nil
}

func filteredMemberID(path string) string {
	start := strings.Index(path, "\"")
	end := strings.LastIndex(path, "\"")
	if start < 0 || end <= start {
		return ""
	}
	return path[start+1 : end]
}
