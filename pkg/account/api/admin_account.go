// Copyright 2022 The Bucketeer Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"context"
	"strconv"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bucketeer-io/bucketeer/pkg/account/command"
	"github.com/bucketeer-io/bucketeer/pkg/account/domain"
	v2as "github.com/bucketeer-io/bucketeer/pkg/account/storage/v2"
	"github.com/bucketeer-io/bucketeer/pkg/locale"
	"github.com/bucketeer-io/bucketeer/pkg/log"
	"github.com/bucketeer-io/bucketeer/pkg/rpc"
	"github.com/bucketeer-io/bucketeer/pkg/storage/v2/mysql"
	accountproto "github.com/bucketeer-io/bucketeer/proto/account"
	environmentproto "github.com/bucketeer-io/bucketeer/proto/environment"
	eventproto "github.com/bucketeer-io/bucketeer/proto/event/domain"
)

func (s *AccountService) GetMe(
	ctx context.Context,
	req *accountproto.GetMeRequest,
) (*accountproto.GetMeResponse, error) {
	t, ok := rpc.GetIDToken(ctx)
	if !ok {
		return nil, localizedError(statusUnauthenticated, locale.JaJP)
	}
	if !verifyEmailFormat(t.Email) {
		s.logger.Error(
			"Email inside IDToken has an invalid format",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.String("email", t.Email))...,
		)
		return nil, localizedError(statusInvalidEmail, locale.JaJP)
	}
	return s.getMe(ctx, t.Email)
}

func (s *AccountService) GetMeByEmail(
	ctx context.Context,
	req *accountproto.GetMeByEmailRequest,
) (*accountproto.GetMeResponse, error) {
	_, err := s.checkAdminRole(ctx)
	if err != nil {
		return nil, err
	}
	if !verifyEmailFormat(req.Email) {
		s.logger.Error(
			"Email inside request has an invalid format",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.String("email", req.Email))...,
		)
		return nil, localizedError(statusInvalidEmail, locale.JaJP)
	}
	return s.getMe(ctx, req.Email)
}

func (s *AccountService) getMe(ctx context.Context, email string) (*accountproto.GetMeResponse, error) {
	projects, err := s.listProjects(ctx)
	if err != nil {
		s.logger.Error(
			"Failed to get project list",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	if len(projects) == 0 {
		s.logger.Error(
			"Could not find any projects",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	environments, err := s.listEnvironments(ctx)
	if err != nil {
		s.logger.Error(
			"Failed to get environment list",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	if len(environments) == 0 {
		s.logger.Error(
			"Could not find any environments",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	// admin account response
	adminAccount, err := s.getAdminAccount(ctx, email)
	if err != nil && status.Code(err) != codes.NotFound {
		return nil, err
	}
	if adminAccount != nil && !adminAccount.Disabled && !adminAccount.Deleted {
		environmentRoles, err := s.makeAdminEnvironmentRoles(projects, environments, accountproto.Account_OWNER)
		if err != nil {
			return nil, err
		}
		return &accountproto.GetMeResponse{
			Account:          adminAccount.Account,
			Email:            adminAccount.Email,
			IsAdmin:          true,
			AdminRole:        accountproto.Account_OWNER,
			Disabled:         false,
			EnvironmentRoles: environmentRoles,
			Deleted:          false,
		}, nil
	}
	// environment acccount response
	environmentRoles, account, err := s.makeEnvironmentRoles(ctx, email, projects, environments)
	if err != nil {
		return nil, err
	}
	return &accountproto.GetMeResponse{
		Account:          account,
		Email:            email,
		IsAdmin:          false,
		AdminRole:        accountproto.Account_UNASSIGNED,
		Disabled:         false,
		EnvironmentRoles: environmentRoles,
		Deleted:          false,
	}, nil
}

func (s *AccountService) makeAdminEnvironmentRoles(
	projects []*environmentproto.Project,
	environments []*environmentproto.Environment,
	adminRole accountproto.Account_Role,
) ([]*accountproto.EnvironmentRole, error) {
	projectSet := s.makeProjectSet(projects)
	environmentRoles := make([]*accountproto.EnvironmentRole, 0)
	for _, e := range environments {
		p, ok := projectSet[e.ProjectId]
		if !ok || p.Disabled {
			continue
		}
		er := &accountproto.EnvironmentRole{Environment: e, Role: adminRole}
		if p.Trial {
			er.TrialProject = true
			er.TrialStartedAt = p.CreatedAt
		}
		environmentRoles = append(environmentRoles, er)
	}
	if len(environmentRoles) == 0 {
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return environmentRoles, nil
}

// FIXME: remove *accountproto.Account response after WebUI supports environment feature and removes the dependency
func (s *AccountService) makeEnvironmentRoles(
	ctx context.Context,
	email string,
	projects []*environmentproto.Project,
	environments []*environmentproto.Environment,
) ([]*accountproto.EnvironmentRole, *accountproto.Account, error) {
	projectSet := s.makeProjectSet(projects)
	var lastAccount *accountproto.Account
	environmentRoles := make([]*accountproto.EnvironmentRole, 0, len(environments))
	for _, e := range environments {
		p, ok := projectSet[e.ProjectId]
		if !ok || p.Disabled {
			continue
		}
		account, err := s.getAccount(ctx, email, e.Namespace)
		if err != nil && status.Code(err) != codes.NotFound {
			return nil, nil, err
		}
		if account == nil || account.Disabled || account.Deleted {
			continue
		}
		lastAccount = account.Account
		er := &accountproto.EnvironmentRole{Environment: e, Role: account.Role}
		if p.Trial {
			er.TrialProject = true
			er.TrialStartedAt = p.CreatedAt
		}
		environmentRoles = append(environmentRoles, er)
	}
	if len(environmentRoles) == 0 {
		return nil, nil, localizedError(statusNotFound, locale.JaJP)
	}
	return environmentRoles, lastAccount, nil
}

func (s *AccountService) CreateAdminAccount(
	ctx context.Context,
	req *accountproto.CreateAdminAccountRequest,
) (*accountproto.CreateAdminAccountResponse, error) {
	editor, err := s.checkAdminRole(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateCreateAdminAccountRequest(req); err != nil {
		s.logger.Error(
			"Failed to create admin account",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, err
	}
	account, err := domain.NewAccount(req.Command.Email, accountproto.Account_OWNER)
	if err != nil {
		s.logger.Error(
			"Failed to create a new admin account",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	environments, err := s.listEnvironments(ctx)
	if err != nil {
		s.logger.Error(
			"Failed to get environment list",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	// check if an Account that has the same email already exists in any environment
	accountStorage := v2as.NewAccountStorage(s.mysqlClient)
	for _, env := range environments {
		_, err := accountStorage.GetAccount(ctx, account.Id, env.Namespace)
		if err == nil {
			return nil, localizedError(statusAlreadyExists, locale.JaJP)
		}
		if err != v2as.ErrAccountNotFound {
			return nil, err
		}
	}
	tx, err := s.mysqlClient.BeginTx(ctx)
	if err != nil {
		s.logger.Error(
			"Failed to begin transaction",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	err = s.mysqlClient.RunInTransaction(ctx, tx, func() error {
		adminAccountStorage := v2as.NewAdminAccountStorage(tx)
		handler := command.NewAdminAccountCommandHandler(editor, account, s.publisher)
		if err := handler.Handle(ctx, req.Command); err != nil {
			return err
		}
		return adminAccountStorage.CreateAdminAccount(ctx, account)
	})
	if err != nil {
		if err == v2as.ErrAdminAccountAlreadyExists {
			return nil, localizedError(statusAlreadyExists, locale.JaJP)
		}
		s.logger.Error(
			"Failed to create admin account",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return &accountproto.CreateAdminAccountResponse{}, nil
}

func (s *AccountService) EnableAdminAccount(
	ctx context.Context,
	req *accountproto.EnableAdminAccountRequest,
) (*accountproto.EnableAdminAccountResponse, error) {
	editor, err := s.checkAdminRole(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateEnableAdminAccountRequest(req); err != nil {
		s.logger.Error(
			"Failed to enable admin account",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, err
	}
	if err := s.updateAdminAccountMySQL(ctx, editor, req.Id, req.Command); err != nil {
		if err == v2as.ErrAdminAccountNotFound || err == v2as.ErrAdminAccountUnexpectedAffectedRows {
			return nil, localizedError(statusNotFound, locale.JaJP)
		}
		s.logger.Error(
			"Failed to enable admin account",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return &accountproto.EnableAdminAccountResponse{}, nil
}

func (s *AccountService) DisableAdminAccount(
	ctx context.Context,
	req *accountproto.DisableAdminAccountRequest,
) (*accountproto.DisableAdminAccountResponse, error) {
	editor, err := s.checkAdminRole(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateDisableAdminAccountRequest(req); err != nil {
		s.logger.Error(
			"Failed to disable admin account",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, err
	}
	if err := s.updateAdminAccountMySQL(ctx, editor, req.Id, req.Command); err != nil {
		if err == v2as.ErrAdminAccountNotFound || err == v2as.ErrAdminAccountUnexpectedAffectedRows {
			return nil, localizedError(statusNotFound, locale.JaJP)
		}
		s.logger.Error(
			"Failed to disable admin account",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return &accountproto.DisableAdminAccountResponse{}, nil
}

func (s *AccountService) updateAdminAccountMySQL(
	ctx context.Context,
	editor *eventproto.Editor,
	id string,
	cmd command.Command,
) error {
	tx, err := s.mysqlClient.BeginTx(ctx)
	if err != nil {
		s.logger.Error(
			"Failed to begin transaction",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
			)...,
		)
		return err
	}
	return s.mysqlClient.RunInTransaction(ctx, tx, func() error {
		adminAccountStorage := v2as.NewAdminAccountStorage(tx)
		account, err := adminAccountStorage.GetAdminAccount(ctx, id)
		if err != nil {
			return err
		}
		handler := command.NewAdminAccountCommandHandler(editor, account, s.publisher)
		if err := handler.Handle(ctx, cmd); err != nil {
			return err
		}
		return adminAccountStorage.UpdateAdminAccount(ctx, account)
	})
}

func (s *AccountService) ConvertAccount(
	ctx context.Context,
	req *accountproto.ConvertAccountRequest,
) (*accountproto.ConvertAccountResponse, error) {
	editor, err := s.checkAdminRole(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateConvertAccountRequest(req); err != nil {
		s.logger.Error(
			"Failed to get account",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, err
	}
	account, err := domain.NewAccount(req.Id, accountproto.Account_OWNER)
	if err != nil {
		s.logger.Error(
			"Failed to create a new admin account",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	environments, err := s.listEnvironments(ctx)
	if err != nil {
		s.logger.Error(
			"Failed to get environment list",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	deleteAccountCommand := &accountproto.DeleteAccountCommand{}
	createAdminAccountCommand := &accountproto.CreateAdminAccountCommand{Email: req.Id}
	tx, err := s.mysqlClient.BeginTx(ctx)
	if err != nil {
		s.logger.Error(
			"Failed to begin transaction",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	err = s.mysqlClient.RunInTransaction(ctx, tx, func() error {
		accountStorage := v2as.NewAccountStorage(tx)
		var existedAccountCount int
		for _, env := range environments {
			existedAccount, err := accountStorage.GetAccount(ctx, account.Id, env.Namespace)
			if err != nil {
				if err == v2as.ErrAccountNotFound {
					continue
				}
				return err
			}
			existedAccountCount++
			handler := command.NewAccountCommandHandler(
				editor,
				existedAccount,
				s.publisher,
				env.Namespace,
			)
			if err := handler.Handle(ctx, deleteAccountCommand); err != nil {
				return err
			}
			if err := accountStorage.UpdateAccount(ctx, existedAccount, env.Namespace); err != nil {
				return err
			}
		}
		if existedAccountCount == 0 {
			return v2as.ErrAccountNotFound
		}
		adminAccountStorage := v2as.NewAdminAccountStorage(tx)
		handler := command.NewAdminAccountCommandHandler(editor, account, s.publisher)
		if err := handler.Handle(ctx, createAdminAccountCommand); err != nil {
			return err
		}
		return adminAccountStorage.CreateAdminAccount(ctx, account)
	})
	if err != nil {
		if err == v2as.ErrAccountNotFound {
			return nil, localizedError(statusNotFound, locale.JaJP)
		}
		if err == v2as.ErrAdminAccountAlreadyExists {
			return nil, localizedError(statusAlreadyExists, locale.JaJP)
		}
		s.logger.Error(
			"Failed to convert account",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, err
	}
	return &accountproto.ConvertAccountResponse{}, nil
}

func (s *AccountService) GetAdminAccount(
	ctx context.Context,
	req *accountproto.GetAdminAccountRequest,
) (*accountproto.GetAdminAccountResponse, error) {
	_, err := s.checkAdminRole(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateGetAdminAccountRequest(req); err != nil {
		s.logger.Error(
			"Failed to get admin account",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, err
	}
	account, err := s.getAdminAccount(ctx, req.Email)
	if err != nil {
		return nil, err
	}
	return &accountproto.GetAdminAccountResponse{Account: account.Account}, nil
}

func (s *AccountService) getAdminAccount(ctx context.Context, email string) (*domain.Account, error) {
	adminAccountStorage := v2as.NewAdminAccountStorage(s.mysqlClient)
	account, err := adminAccountStorage.GetAdminAccount(ctx, email)
	if err != nil {
		if err == v2as.ErrAdminAccountNotFound {
			return nil, localizedError(statusNotFound, locale.JaJP)
		}
		s.logger.Error(
			"Failed to get admin account",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("email", email),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return account, nil
}

func (s *AccountService) ListAdminAccounts(
	ctx context.Context,
	req *accountproto.ListAdminAccountsRequest,
) (*accountproto.ListAdminAccountsResponse, error) {
	_, err := s.checkAdminRole(ctx)
	if err != nil {
		return nil, err
	}
	whereParts := []mysql.WherePart{mysql.NewFilter("deleted", "=", false)}
	if req.Disabled != nil {
		whereParts = append(whereParts, mysql.NewFilter("disabled", "=", req.Disabled.Value))
	}
	if req.SearchKeyword != "" {
		whereParts = append(whereParts, mysql.NewSearchQuery([]string{"email"}, req.SearchKeyword))
	}
	orders, err := s.newAdminAccountListOrders(req.OrderBy, req.OrderDirection)
	if err != nil {
		s.logger.Error(
			"Invalid argument",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, err
	}
	limit := int(req.PageSize)
	cursor := req.Cursor
	if cursor == "" {
		cursor = "0"
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil {
		return nil, localizedError(statusInvalidCursor, locale.JaJP)
	}
	adminAccountStorage := v2as.NewAdminAccountStorage(s.mysqlClient)
	accounts, nextCursor, totalCount, err := adminAccountStorage.ListAdminAccounts(
		ctx,
		whereParts,
		orders,
		limit,
		offset,
	)
	if err != nil {
		s.logger.Error(
			"Failed to list admin accounts",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return &accountproto.ListAdminAccountsResponse{
		Accounts:   accounts,
		Cursor:     strconv.Itoa(nextCursor),
		TotalCount: totalCount,
	}, nil
}

func (s *AccountService) newAdminAccountListOrders(
	orderBy accountproto.ListAdminAccountsRequest_OrderBy,
	orderDirection accountproto.ListAdminAccountsRequest_OrderDirection,
) ([]*mysql.Order, error) {
	var column string
	switch orderBy {
	case accountproto.ListAdminAccountsRequest_DEFAULT,
		accountproto.ListAdminAccountsRequest_EMAIL:
		column = "email"
	case accountproto.ListAdminAccountsRequest_CREATED_AT:
		column = "created_at"
	case accountproto.ListAdminAccountsRequest_UPDATED_AT:
		column = "updated_at"
	default:
		return nil, localizedError(statusInvalidOrderBy, locale.JaJP)
	}
	direction := mysql.OrderDirectionAsc
	if orderDirection == accountproto.ListAdminAccountsRequest_DESC {
		direction = mysql.OrderDirectionDesc
	}
	return []*mysql.Order{mysql.NewOrder(column, direction)}, nil
}
