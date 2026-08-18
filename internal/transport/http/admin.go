package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/access"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// AdminDeps bundles the user, group and grant management dependencies.
type AdminDeps struct {
	CreateUser    *command.CreateUser
	UpdateUser    *command.UpdateUser
	ResetPassword *command.ResetPassword
	DeleteUser    *command.DeleteUser
	SetUserGroups *command.SetUserGroups
	ManageAccess  *command.ManageAccess
	// MFA resets another account's second factor: the lost-phone path
	// (AUTH-04). It lives here rather than with the self-service enrolment
	// because it is an administrator acting on somebody else's account.
	MFA    *command.MFA
	Users  ports.UserRepository
	Access ports.AccessRepository
	Audit  ports.AuditWriter
}

// --- payloads ----------------------------------------------------------

type userResponse struct {
	ID                 string   `json:"id"`
	Username           string   `json:"username"`
	Email              string   `json:"email"`
	DisplayName        string   `json:"display_name"`
	Role               string   `json:"role"`
	IsActive           bool     `json:"is_active"`
	TOTPEnabled        bool     `json:"totp_enabled"`
	MustChangePassword bool     `json:"must_change_password"`
	Groups             []string `json:"groups,omitempty"`
	LastLoginAt        *string  `json:"last_login_at,omitempty"`
	CreatedAt          string   `json:"created_at"`
}

type createUserRequest struct {
	Username     string   `json:"username"`
	Email        string   `json:"email"`
	DisplayName  string   `json:"display_name"`
	Role         string   `json:"role"`
	TempPassword string   `json:"temp_password"`
	GroupIDs     []string `json:"group_ids"`
}

type updateUserRequest struct {
	DisplayName *string `json:"display_name"`
	Email       *string `json:"email"`
	Role        *string `json:"role"`
	IsActive    *bool   `json:"is_active"`
}

type passwordResetRequest struct {
	TempPassword string `json:"temp_password"`
}

type groupsRequest struct {
	GroupIDs []string `json:"group_ids"`
}

type createGroupRequest struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	AutoRule    map[string]any `json:"auto_rule,omitempty"`
}

type createGrantRequest struct {
	UserGroupID string `json:"user_group_id"`
	VMGroupID   string `json:"vm_group_id"`
}

// --- users -------------------------------------------------------------

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	filter := ports.UserFilter{
		Query: r.URL.Query().Get("q"),
		Role:  r.URL.Query().Get("role"),
	}
	if v := r.URL.Query().Get("active"); v != "" {
		active := v == "true"
		filter.Active = &active
	}

	users, err := s.admin.Users.List(r.Context(), filter)
	if err != nil {
		s.serverError(w, r, err, "Could not list users.")
		return
	}
	out := make([]userResponse, 0, len(users))
	for _, u := range users {
		out = append(out, toUserResponse(u, nil))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": out, "meta": map[string]any{"total": len(out)}})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.Username == "" || req.Email == "" || req.TempPassword == "" {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", "Missing required fields.",
			map[string]string{"username": "required", "email": "required", "temp_password": "required"})
		return
	}
	groupIDs, err := parseUUIDs(req.GroupIDs)
	if err != nil {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", "Invalid group id.",
			map[string]string{"group_ids": "must be UUIDs"})
		return
	}

	user, err := s.admin.CreateUser.Handle(r.Context(), command.CreateUserInput{
		Actor:        s.actor(r),
		Username:     req.Username,
		Email:        req.Email,
		DisplayName:  req.DisplayName,
		Role:         identity.Role(req.Role),
		TempPassword: req.TempPassword,
		GroupIDs:     groupIDs,
	})
	if err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, toUserResponse(user, nil))
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	user, err := s.admin.Users.GetByID(r.Context(), id)
	if err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	groups, err := s.admin.Access.UserGroupNames(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err, "Could not load group membership.")
		return
	}
	WriteJSON(w, http.StatusOK, toUserResponse(user, groups))
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	var req updateUserRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	in := command.UpdateUserInput{
		Actor: s.actor(r), UserID: id,
		DisplayName: req.DisplayName, Email: req.Email, IsActive: req.IsActive,
	}
	if req.Role != nil {
		role := identity.Role(*req.Role)
		in.Role = &role
	}

	user, err := s.admin.UpdateUser.Handle(r.Context(), in)
	if err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toUserResponse(user, nil))
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	var req passwordResetRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	err := s.admin.ResetPassword.Handle(r.Context(), command.ResetPasswordInput{
		Actor: s.actor(r), UserID: id, TempPassword: req.TempPassword,
	})
	if err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	err := s.admin.DeleteUser.Handle(r.Context(), command.DeleteUserInput{
		Actor: s.actor(r), UserID: id,
	})
	if err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetUserGroups(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	var req groupsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	groupIDs, err := parseUUIDs(req.GroupIDs)
	if err != nil {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", "Invalid group id.",
			map[string]string{"group_ids": "must be UUIDs"})
		return
	}
	err = s.admin.SetUserGroups.Handle(r.Context(), command.SetUserGroupsInput{
		Actor: s.actor(r), UserID: id, GroupIDs: groupIDs,
	})
	if err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- groups and grants -------------------------------------------------

func (s *Server) handleListUserGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.admin.Access.ListUserGroups(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list user groups.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": groups})
}

func (s *Server) handleCreateUserGroup(w http.ResponseWriter, r *http.Request) {
	var req createGroupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	g, err := s.admin.ManageAccess.CreateUserGroup(r.Context(), command.GroupInput{
		Actor: s.actor(r), Name: req.Name, Description: req.Description,
	})
	if err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, g)
}

func (s *Server) handleDeleteUserGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "groupID")
	if !ok {
		return
	}
	if err := s.admin.ManageAccess.DeleteUserGroup(r.Context(), s.actor(r), id); err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListVMGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.admin.Access.ListVMGroups(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list VM groups.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": groups})
}

func (s *Server) handleCreateVMGroup(w http.ResponseWriter, r *http.Request) {
	var req createGroupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	g, err := s.admin.ManageAccess.CreateVMGroup(r.Context(), command.GroupInput{
		Actor: s.actor(r), Name: req.Name, Description: req.Description, AutoRule: req.AutoRule,
	})
	if err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, g)
}

func (s *Server) handleListVMGroupMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "groupID")
	if !ok {
		return
	}
	ids, err := s.admin.Access.VMGroupMemberIDs(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err, "Could not list group members.")
		return
	}
	out := make([]string, 0, len(ids))
	for _, vmID := range ids {
		out = append(out, vmID.String())
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

type setMembersRequest struct {
	VMIDs []string `json:"vm_ids"`
}

func (s *Server) handleSetVMGroupMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "groupID")
	if !ok {
		return
	}
	var req setMembersRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	ids := make([]uuid.UUID, 0, len(req.VMIDs))
	for _, raw := range req.VMIDs {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
				"One of the supplied VM identifiers is not a valid UUID.")
			return
		}
		ids = append(ids, parsed)
	}
	if err := s.admin.ManageAccess.SetVMGroupMembers(r.Context(), s.actor(r), id, ids); err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteVMGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "groupID")
	if !ok {
		return
	}
	if err := s.admin.ManageAccess.DeleteVMGroup(r.Context(), s.actor(r), id); err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	grants, err := s.admin.Access.ListGrants(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list grants.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": grants})
}

func (s *Server) handleCreateGrant(w http.ResponseWriter, r *http.Request) {
	var req createGrantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	userGroupID, err1 := uuid.Parse(req.UserGroupID)
	vmGroupID, err2 := uuid.Parse(req.VMGroupID)
	if err1 != nil || err2 != nil {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", "Both group ids are required.",
			map[string]string{"user_group_id": "must be a UUID", "vm_group_id": "must be a UUID"})
		return
	}

	g, err := s.admin.ManageAccess.CreateGrant(r.Context(), command.GrantInput{
		Actor: s.actor(r), UserGroupID: userGroupID, VMGroupID: vmGroupID,
	})
	if err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, g)
}

func (s *Server) handleDeleteGrant(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "grantID")
	if !ok {
		return
	}
	if err := s.admin.ManageAccess.DeleteGrant(r.Context(), s.actor(r), id); err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers -----------------------------------------------------------

func (s *Server) actor(r *http.Request) command.Actor {
	a := command.Actor{
		IP:        r.RemoteAddr,
		UserAgent: r.UserAgent(),
		RequestID: middleware.GetReqID(r.Context()),
	}
	if p, ok := PrincipalFrom(r.Context()); ok {
		a.UserID = p.UserID
		a.Username = p.Username
	}
	return a
}

func (s *Server) pathUUID(w http.ResponseWriter, r *http.Request, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) writeAdminError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
	case errors.Is(err, ports.ErrConflict):
		WriteProblem(w, r, http.StatusConflict, "conflict", "That name is already taken.")
	case errors.Is(err, identity.ErrCannotDeleteSelf):
		WriteProblem(w, r, http.StatusConflict, "user.self_delete",
			"You cannot delete your own account. Ask another administrator to do it.")
	case errors.Is(err, identity.ErrLastAdmin):
		WriteProblem(w, r, http.StatusConflict, "user.last_admin",
			"This is the last administrator who can sign in. Give another account the administrator role first.")
	case errors.Is(err, identity.ErrInvalidRole):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", "Unknown role.",
			map[string]string{"role": "must be admin, operator, readonly or auditor"})
	case errors.Is(err, identity.ErrWeakPassword):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(),
			map[string]string{"temp_password": "does not meet the password policy"})
	case errors.Is(err, access.ErrInvalidName):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", "Invalid group name.",
			map[string]string{"name": "required, at most 64 characters"})
	default:
		s.serverError(w, r, err, "The request could not be completed.")
	}
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error, detail string) {
	s.log.Error().Err(err).Str("path", r.URL.Path).Msg("request failed")
	WriteProblem(w, r, http.StatusInternalServerError, "internal", detail)
}

func parseUUIDs(in []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(in))
	for _, s := range in {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

func toUserResponse(u *identity.User, groups []string) userResponse {
	resp := userResponse{
		ID: u.ID.String(), Username: u.Username, Email: u.Email,
		DisplayName: u.DisplayName, Role: string(u.Role), IsActive: u.IsActive,
		TOTPEnabled: u.TOTPEnabled, MustChangePassword: u.MustChangePassword,
		Groups:    groups,
		CreatedAt: u.CreatedAt.Format(timeFormat),
	}
	if !u.LastLoginAt.IsZero() {
		formatted := u.LastLoginAt.Format(timeFormat)
		resp.LastLoginAt = &formatted
	}
	return resp
}

const timeFormat = "2006-01-02T15:04:05Z07:00"
