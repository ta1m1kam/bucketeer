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
	"errors"
	"strconv"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountclient "github.com/bucketeer-io/bucketeer/pkg/account/client"
	experimentclient "github.com/bucketeer-io/bucketeer/pkg/experiment/client"
	featureclient "github.com/bucketeer-io/bucketeer/pkg/feature/client"
	"github.com/bucketeer-io/bucketeer/pkg/locale"
	"github.com/bucketeer-io/bucketeer/pkg/log"
	"github.com/bucketeer-io/bucketeer/pkg/pubsub/publisher"
	"github.com/bucketeer-io/bucketeer/pkg/push/command"
	"github.com/bucketeer-io/bucketeer/pkg/push/domain"
	v2ps "github.com/bucketeer-io/bucketeer/pkg/push/storage/v2"
	"github.com/bucketeer-io/bucketeer/pkg/role"
	"github.com/bucketeer-io/bucketeer/pkg/storage/v2/mysql"
	accountproto "github.com/bucketeer-io/bucketeer/proto/account"
	eventproto "github.com/bucketeer-io/bucketeer/proto/event/domain"
	pushproto "github.com/bucketeer-io/bucketeer/proto/push"
)

const listRequestSize = 500

var errTagDuplicated = errors.New("push: tag is duplicated")

type options struct {
	logger *zap.Logger
}

type Option func(*options)

func WithLogger(l *zap.Logger) Option {
	return func(opts *options) {
		opts.logger = l
	}
}

type PushService struct {
	mysqlClient      mysql.Client
	featureClient    featureclient.Client
	experimentClient experimentclient.Client
	accountClient    accountclient.Client
	publisher        publisher.Publisher
	opts             *options
	logger           *zap.Logger
}

func NewPushService(
	mysqlClient mysql.Client,
	featureClient featureclient.Client,
	experimentClient experimentclient.Client,
	accountClient accountclient.Client,
	publisher publisher.Publisher,
	opts ...Option,
) *PushService {
	dopts := &options{
		logger: zap.NewNop(),
	}
	for _, opt := range opts {
		opt(dopts)
	}
	return &PushService{
		mysqlClient:      mysqlClient,
		featureClient:    featureClient,
		experimentClient: experimentClient,
		accountClient:    accountClient,
		publisher:        publisher,
		opts:             dopts,
		logger:           dopts.logger.Named("api"),
	}
}

func (s *PushService) Register(server *grpc.Server) {
	pushproto.RegisterPushServiceServer(server, s)
}

func (s *PushService) CreatePush(
	ctx context.Context,
	req *pushproto.CreatePushRequest,
) (*pushproto.CreatePushResponse, error) {
	editor, err := s.checkRole(ctx, accountproto.Account_EDITOR, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	if err := s.validateCreatePushRequest(req); err != nil {
		return nil, err
	}
	push, err := domain.NewPush(req.Command.Name, req.Command.FcmApiKey, req.Command.Tags)
	if err != nil {
		s.logger.Error(
			"Failed to create a new push",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
				zap.Strings("tags", req.Command.Tags),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	pushes, err := s.listAllPushes(ctx, req.EnvironmentNamespace)
	if err != nil {
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	if s.containsFCMKey(ctx, pushes, req.Command.FcmApiKey) {
		return nil, localizedError(statusFCMKeyAlreadyExists, locale.JaJP)
	}
	err = s.containsTags(ctx, pushes, req.Command.Tags)
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil, localizedError(statusTagAlreadyExists, locale.JaJP)
		}
		s.logger.Error(
			"Failed to validate tag existence",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
				zap.Strings("tags", req.Command.Tags),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
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
		pushStorage := v2ps.NewPushStorage(tx)
		if err := pushStorage.CreatePush(ctx, push, req.EnvironmentNamespace); err != nil {
			return err
		}
		handler := command.NewPushCommandHandler(editor, push, s.publisher, req.EnvironmentNamespace)
		if err := handler.Handle(ctx, req.Command); err != nil {
			return err
		}
		return nil

	})
	if err != nil {
		if err == v2ps.ErrPushAlreadyExists {
			return nil, localizedError(statusAlreadyExists, locale.JaJP)
		}
		s.logger.Error(
			"Failed to create push",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return &pushproto.CreatePushResponse{}, nil
}

func (s *PushService) validateCreatePushRequest(req *pushproto.CreatePushRequest) error {
	if req.Command == nil {
		return localizedError(statusNoCommand, locale.JaJP)
	}
	if req.Command.FcmApiKey == "" {
		return localizedError(statusFCMAPIKeyRequired, locale.JaJP)
	}
	if len(req.Command.Tags) == 0 {
		return localizedError(statusTagsRequired, locale.JaJP)
	}
	if req.Command.Name == "" {
		return localizedError(statusNameRequired, locale.JaJP)
	}
	return nil
}

func (s *PushService) UpdatePush(
	ctx context.Context,
	req *pushproto.UpdatePushRequest,
) (*pushproto.UpdatePushResponse, error) {
	editor, err := s.checkRole(ctx, accountproto.Account_EDITOR, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	if err := s.validateUpdatePushRequest(ctx, req); err != nil {
		return nil, err
	}
	commands := s.createUpdatePushCommands(req)
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
		pushStorage := v2ps.NewPushStorage(tx)
		push, err := pushStorage.GetPush(ctx, req.Id, req.EnvironmentNamespace)
		if err != nil {
			return err
		}
		handler := command.NewPushCommandHandler(editor, push, s.publisher, req.EnvironmentNamespace)
		for _, command := range commands {
			if err := handler.Handle(ctx, command); err != nil {
				return err
			}
		}
		return pushStorage.UpdatePush(ctx, push, req.EnvironmentNamespace)
	})
	if err != nil {
		if err == v2ps.ErrPushNotFound || err == v2ps.ErrPushUnexpectedAffectedRows {
			return nil, localizedError(statusNotFound, locale.JaJP)
		}
		s.logger.Error(
			"Failed to update push",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
				zap.String("id", req.Id),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return &pushproto.UpdatePushResponse{}, nil
}

func (s *PushService) validateUpdatePushRequest(ctx context.Context, req *pushproto.UpdatePushRequest) error {
	if req.Id == "" {
		return localizedError(statusIDRequired, locale.JaJP)
	}
	if s.isNoUpdatePushCommand(req) {
		return localizedError(statusNoCommand, locale.JaJP)
	}
	if req.DeletePushTagsCommand != nil && len(req.DeletePushTagsCommand.Tags) == 0 {
		return localizedError(statusTagsRequired, locale.JaJP)
	}
	if err := s.validateAddPushTagsCommand(ctx, req); err != nil {
		return err
	}
	if req.RenamePushCommand != nil && req.RenamePushCommand.Name == "" {
		return localizedError(statusNameRequired, locale.JaJP)
	}
	return nil
}

func (s *PushService) validateAddPushTagsCommand(ctx context.Context, req *pushproto.UpdatePushRequest) error {
	if req.AddPushTagsCommand == nil {
		return nil
	}
	if len(req.AddPushTagsCommand.Tags) == 0 {
		return localizedError(statusTagsRequired, locale.JaJP)
	}
	pushes, err := s.listAllPushes(ctx, req.EnvironmentNamespace)
	if err != nil {
		return localizedError(statusInternal, locale.JaJP)
	}
	err = s.containsTags(ctx, pushes, req.AddPushTagsCommand.Tags)
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return localizedError(statusTagAlreadyExists, locale.JaJP)
		}
		s.logger.Error(
			"Failed to validate tag existence",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
				zap.String("id", req.Id),
				zap.Strings("tags", req.AddPushTagsCommand.Tags),
			)...,
		)
		return localizedError(statusInternal, locale.JaJP)
	}
	return nil
}

func (s *PushService) isNoUpdatePushCommand(req *pushproto.UpdatePushRequest) bool {
	return req.AddPushTagsCommand == nil &&
		req.DeletePushTagsCommand == nil &&
		req.RenamePushCommand == nil
}

func (s *PushService) DeletePush(
	ctx context.Context,
	req *pushproto.DeletePushRequest,
) (*pushproto.DeletePushResponse, error) {
	editor, err := s.checkRole(ctx, accountproto.Account_EDITOR, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	if err := validateDeletePushRequest(req); err != nil {
		return nil, err
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
		pushStorage := v2ps.NewPushStorage(tx)
		push, err := pushStorage.GetPush(ctx, req.Id, req.EnvironmentNamespace)
		if err != nil {
			return err
		}
		handler := command.NewPushCommandHandler(editor, push, s.publisher, req.EnvironmentNamespace)
		if err := handler.Handle(ctx, req.Command); err != nil {
			return err
		}
		return pushStorage.UpdatePush(ctx, push, req.EnvironmentNamespace)
	})
	if err != nil {
		if err == v2ps.ErrPushNotFound || err == v2ps.ErrPushUnexpectedAffectedRows {
			return nil, localizedError(statusNotFound, locale.JaJP)
		}
		s.logger.Error(
			"Failed to delete push",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("id", req.Id),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return &pushproto.DeletePushResponse{}, nil
}

func validateDeletePushRequest(req *pushproto.DeletePushRequest) error {
	if req.Id == "" {
		return localizedError(statusIDRequired, locale.JaJP)
	}
	if req.Command == nil {
		return localizedError(statusNoCommand, locale.JaJP)
	}
	return nil
}

func (s *PushService) createUpdatePushCommands(req *pushproto.UpdatePushRequest) []command.Command {
	commands := make([]command.Command, 0)
	if req.DeletePushTagsCommand != nil {
		commands = append(commands, req.DeletePushTagsCommand)
	}
	if req.AddPushTagsCommand != nil {
		commands = append(commands, req.AddPushTagsCommand)
	}
	if req.RenamePushCommand != nil {
		commands = append(commands, req.RenamePushCommand)
	}
	return commands
}

func (s *PushService) containsTags(ctx context.Context, pushes []*pushproto.Push, tags []string) error {
	m, err := s.tagMap(pushes)
	if err != nil {
		return err
	}
	for _, t := range tags {
		if _, ok := m[t]; ok {
			return localizedError(statusTagAlreadyExists, locale.JaJP)
		}
	}
	return nil
}

func (s *PushService) containsFCMKey(ctx context.Context, pushes []*pushproto.Push, fcmAPIKey string) bool {
	for _, push := range pushes {
		if push.FcmApiKey == fcmAPIKey {
			return true
		}
	}
	return false
}

func (s *PushService) tagMap(pushes []*pushproto.Push) (map[string]struct{}, error) {
	m := make(map[string]struct{})
	for _, p := range pushes {
		for _, t := range p.Tags {
			if _, ok := m[t]; ok {
				return nil, errTagDuplicated
			}
			m[t] = struct{}{}
		}
	}
	return m, nil
}

func (s *PushService) listAllPushes(ctx context.Context, environmentNamespace string) ([]*pushproto.Push, error) {
	pushes := []*pushproto.Push{}
	cursor := ""
	whereParts := []mysql.WherePart{
		mysql.NewFilter("deleted", "=", false),
		mysql.NewFilter("environment_namespace", "=", environmentNamespace),
	}
	for {
		ps, curCursor, _, err := s.listPushes(
			ctx,
			listRequestSize,
			cursor,
			environmentNamespace,
			whereParts,
			nil,
		)
		if err != nil {
			return nil, err
		}
		pushes = append(pushes, ps...)
		psSize := len(ps)
		if psSize == 0 || psSize < listRequestSize {
			return pushes, nil
		}
		cursor = curCursor
	}
}

func (s *PushService) ListPushes(
	ctx context.Context,
	req *pushproto.ListPushesRequest,
) (*pushproto.ListPushesResponse, error) {
	_, err := s.checkRole(ctx, accountproto.Account_VIEWER, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	whereParts := []mysql.WherePart{
		mysql.NewFilter("deleted", "=", false),
		mysql.NewFilter("environment_namespace", "=", req.EnvironmentNamespace),
	}
	if req.SearchKeyword != "" {
		whereParts = append(whereParts, mysql.NewSearchQuery([]string{"name"}, req.SearchKeyword))
	}
	orders, err := s.newListOrders(req.OrderBy, req.OrderDirection)
	if err != nil {
		s.logger.Error(
			"Invalid argument",
			log.FieldsFromImcomingContext(ctx).AddFields(zap.Error(err))...,
		)
		return nil, err
	}
	pushes, cursor, totalCount, err := s.listPushes(
		ctx,
		req.PageSize,
		req.Cursor,
		req.EnvironmentNamespace,
		whereParts,
		orders,
	)
	if err != nil {
		return nil, err
	}
	return &pushproto.ListPushesResponse{
		Pushes:     pushes,
		Cursor:     cursor,
		TotalCount: totalCount,
	}, nil
}

func (s *PushService) newListOrders(
	orderBy pushproto.ListPushesRequest_OrderBy,
	orderDirection pushproto.ListPushesRequest_OrderDirection,
) ([]*mysql.Order, error) {
	var column string
	switch orderBy {
	case pushproto.ListPushesRequest_DEFAULT,
		pushproto.ListPushesRequest_NAME:
		column = "name"
	case pushproto.ListPushesRequest_CREATED_AT:
		column = "created_at"
	case pushproto.ListPushesRequest_UPDATED_AT:
		column = "updated_at"
	default:
		return nil, localizedError(statusInvalidOrderBy, locale.JaJP)
	}
	direction := mysql.OrderDirectionAsc
	if orderDirection == pushproto.ListPushesRequest_DESC {
		direction = mysql.OrderDirectionDesc
	}
	return []*mysql.Order{mysql.NewOrder(column, direction)}, nil
}

func (s *PushService) listPushes(
	ctx context.Context,
	pageSize int64,
	cursor string,
	environmentNamespace string,
	whereParts []mysql.WherePart,
	orders []*mysql.Order,
) ([]*pushproto.Push, string, int64, error) {
	limit := int(pageSize)
	if cursor == "" {
		cursor = "0"
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil {
		return nil, "", 0, localizedError(statusInvalidCursor, locale.JaJP)
	}
	pushStorage := v2ps.NewPushStorage(s.mysqlClient)
	pushes, nextCursor, totalCount, err := pushStorage.ListPushes(
		ctx,
		whereParts,
		orders,
		limit,
		offset,
	)
	if err != nil {
		s.logger.Error(
			"Failed to list pushes",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", environmentNamespace),
			)...,
		)
		return nil, "", 0, localizedError(statusInternal, locale.JaJP)
	}
	return pushes, strconv.Itoa(nextCursor), totalCount, nil
}

func (s *PushService) checkRole(
	ctx context.Context,
	requiredRole accountproto.Account_Role,
	environmentNamespace string,
) (*eventproto.Editor, error) {
	editor, err := role.CheckRole(ctx, requiredRole, func(email string) (*accountproto.GetAccountResponse, error) {
		return s.accountClient.GetAccount(ctx, &accountproto.GetAccountRequest{
			Email:                email,
			EnvironmentNamespace: environmentNamespace,
		})
	})
	if err != nil {
		switch status.Code(err) {
		case codes.Unauthenticated:
			s.logger.Info(
				"Unauthenticated",
				log.FieldsFromImcomingContext(ctx).AddFields(
					zap.Error(err),
					zap.String("environmentNamespace", environmentNamespace),
				)...,
			)
			return nil, localizedError(statusUnauthenticated, locale.JaJP)
		case codes.PermissionDenied:
			s.logger.Info(
				"Permission denied",
				log.FieldsFromImcomingContext(ctx).AddFields(
					zap.Error(err),
					zap.String("environmentNamespace", environmentNamespace),
				)...,
			)
			return nil, localizedError(statusPermissionDenied, locale.JaJP)
		default:
			s.logger.Error(
				"Failed to check role",
				log.FieldsFromImcomingContext(ctx).AddFields(
					zap.Error(err),
					zap.String("environmentNamespace", environmentNamespace),
				)...,
			)
			return nil, localizedError(statusInternal, locale.JaJP)
		}
	}
	return editor, nil
}
