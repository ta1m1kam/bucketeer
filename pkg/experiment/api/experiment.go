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

	"github.com/bucketeer-io/bucketeer/pkg/experiment/command"
	"github.com/bucketeer-io/bucketeer/pkg/experiment/domain"
	v2es "github.com/bucketeer-io/bucketeer/pkg/experiment/storage/v2"
	"github.com/bucketeer-io/bucketeer/pkg/locale"
	"github.com/bucketeer-io/bucketeer/pkg/log"
	"github.com/bucketeer-io/bucketeer/pkg/storage/v2/mysql"
	accountproto "github.com/bucketeer-io/bucketeer/proto/account"
	eventproto "github.com/bucketeer-io/bucketeer/proto/event/domain"
	proto "github.com/bucketeer-io/bucketeer/proto/experiment"
	featureproto "github.com/bucketeer-io/bucketeer/proto/feature"
)

const (
	maxExperimentPeriodDays = 30
	maxExperimentPeriod     = maxExperimentPeriodDays * 24 * 60 * 60
)

func (s *experimentService) GetExperiment(
	ctx context.Context,
	req *proto.GetExperimentRequest,
) (*proto.GetExperimentResponse, error) {
	_, err := s.checkRole(ctx, accountproto.Account_VIEWER, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	if err := validateGetExperimentRequest(req); err != nil {
		return nil, err
	}
	experimentStorage := v2es.NewExperimentStorage(s.mysqlClient)
	experiment, err := experimentStorage.GetExperiment(ctx, req.Id, req.EnvironmentNamespace)
	if err != nil {
		if err == v2es.ErrExperimentNotFound {
			return nil, localizedError(statusNotFound, locale.JaJP)
		}
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return &proto.GetExperimentResponse{
		Experiment: experiment.Experiment,
	}, nil
}

func validateGetExperimentRequest(req *proto.GetExperimentRequest) error {
	if req.Id == "" {
		return localizedError(statusExperimentIDRequired, locale.JaJP)
	}
	return nil
}

func (s *experimentService) ListExperiments(
	ctx context.Context,
	req *proto.ListExperimentsRequest,
) (*proto.ListExperimentsResponse, error) {
	_, err := s.checkRole(ctx, accountproto.Account_VIEWER, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	whereParts := []mysql.WherePart{
		mysql.NewFilter("deleted", "=", false),
		mysql.NewFilter("environment_namespace", "=", req.EnvironmentNamespace),
	}
	if req.Archived != nil {
		whereParts = append(whereParts, mysql.NewFilter("archived", "=", req.Archived.Value))
	}
	if req.FeatureId != "" {
		whereParts = append(whereParts, mysql.NewFilter("feature_id", "=", req.FeatureId))
	}
	if req.FeatureVersion != nil {
		whereParts = append(whereParts, mysql.NewFilter("feature_version", "=", req.FeatureVersion.Value))
	}
	if req.From != 0 {
		whereParts = append(whereParts, mysql.NewFilter("stopped_at", ">=", req.From))
	}
	if req.To != 0 {
		whereParts = append(whereParts, mysql.NewFilter("start_at", "<=", req.To))
	}
	if req.Status != nil {
		whereParts = append(whereParts, mysql.NewFilter("status", "=", req.Status.Value))
	} else if len(req.Statuses) > 0 {
		statuses := make([]interface{}, 0, len(req.Statuses))
		for _, sts := range req.Statuses {
			statuses = append(statuses, sts)
		}
		whereParts = append(whereParts, mysql.NewInFilter("status", statuses))
	}
	if req.Maintainer != "" {
		whereParts = append(whereParts, mysql.NewFilter("maintainer", "=", req.Maintainer))
	}
	if req.SearchKeyword != "" {
		whereParts = append(whereParts, mysql.NewSearchQuery([]string{"name", "description"}, req.SearchKeyword))
	}
	orders, err := s.newExperimentListOrders(req.OrderBy, req.OrderDirection)
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
	experimentStorage := v2es.NewExperimentStorage(s.mysqlClient)
	experiments, nextCursor, totalCount, err := experimentStorage.ListExperiments(
		ctx,
		whereParts,
		orders,
		limit,
		offset,
	)
	if err != nil {
		s.logger.Error(
			"Failed to list experiments",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return &proto.ListExperimentsResponse{
		Experiments: experiments,
		Cursor:      strconv.Itoa(nextCursor),
		TotalCount:  totalCount,
	}, nil
}

func (s *experimentService) newExperimentListOrders(
	orderBy proto.ListExperimentsRequest_OrderBy,
	orderDirection proto.ListExperimentsRequest_OrderDirection,
) ([]*mysql.Order, error) {
	var column string
	switch orderBy {
	case proto.ListExperimentsRequest_DEFAULT,
		proto.ListExperimentsRequest_NAME:
		column = "name"
	case proto.ListExperimentsRequest_CREATED_AT:
		column = "created_at"
	case proto.ListExperimentsRequest_UPDATED_AT:
		column = "updated_at"
	default:
		return nil, localizedError(statusInvalidOrderBy, locale.JaJP)
	}
	direction := mysql.OrderDirectionAsc
	if orderDirection == proto.ListExperimentsRequest_DESC {
		direction = mysql.OrderDirectionDesc
	}
	return []*mysql.Order{mysql.NewOrder(column, direction)}, nil
}

func (s *experimentService) CreateExperiment(
	ctx context.Context,
	req *proto.CreateExperimentRequest,
) (*proto.CreateExperimentResponse, error) {
	editor, err := s.checkRole(ctx, accountproto.Account_EDITOR, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	if err := validateCreateExperimentRequest(req); err != nil {
		return nil, err
	}
	resp, err := s.featureClient.GetFeature(ctx, &featureproto.GetFeatureRequest{
		Id:                   req.Command.FeatureId,
		EnvironmentNamespace: req.EnvironmentNamespace,
	})
	if err != nil {
		if code := status.Code(err); code == codes.NotFound {
			return nil, localizedError(statusFeatureNotFound, locale.JaJP)
		}
		s.logger.Error(
			"Failed to get feature",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	for _, gid := range req.Command.GoalIds {
		_, err := s.getGoalMySQL(ctx, gid, req.EnvironmentNamespace)
		if err != nil {
			if err == v2es.ErrGoalNotFound {
				return nil, localizedError(statusGoalNotFound, locale.JaJP)
			}
			return nil, localizedError(statusInternal, locale.JaJP)
		}
	}
	experiment, err := domain.NewExperiment(
		req.Command.FeatureId,
		resp.Feature.Version,
		resp.Feature.Variations,
		req.Command.GoalIds,
		req.Command.StartAt,
		req.Command.StopAt,
		req.Command.Name,
		req.Command.Description,
		req.Command.BaseVariationId,
		editor.Email,
	)
	if err != nil {
		s.logger.Error(
			"Failed to create a new experiment",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
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
		experimentStorage := v2es.NewExperimentStorage(tx)
		handler := command.NewExperimentCommandHandler(
			editor,
			experiment,
			s.publisher,
			req.EnvironmentNamespace,
		)
		if err := handler.Handle(ctx, req.Command); err != nil {
			return err
		}
		return experimentStorage.CreateExperiment(ctx, experiment, req.EnvironmentNamespace)
	})
	if err != nil {
		if err == v2es.ErrExperimentAlreadyExists {
			return nil, localizedError(statusAlreadyExists, locale.JaJP)
		}
		s.logger.Error(
			"Failed to create experiment",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return &proto.CreateExperimentResponse{
		Experiment: experiment.Experiment,
	}, nil
}

func validateCreateExperimentRequest(req *proto.CreateExperimentRequest) error {
	if req.Command == nil {
		return localizedError(statusNoCommand, locale.JaJP)
	}
	if req.Command.FeatureId == "" {
		return localizedError(statusFeatureIDRequired, locale.JaJP)
	}
	if len(req.Command.GoalIds) == 0 {
		return localizedError(statusGoalIDRequired, locale.JaJP)
	}
	for _, gid := range req.Command.GoalIds {
		if gid == "" {
			return localizedError(statusGoalIDRequired, locale.JaJP)
		}
	}
	if err := validateExperimentPeriod(req.Command.StartAt, req.Command.StopAt); err != nil {
		return err
	}
	// TODO: validate name empty check
	return nil
}

func validateExperimentPeriod(startAt, stopAt int64) error {
	period := stopAt - startAt
	if period <= 0 || period > int64(maxExperimentPeriod) {
		return localizedError(statusPeriodTooLong, locale.JaJP)
	}
	return nil
}

func (s *experimentService) UpdateExperiment(
	ctx context.Context,
	req *proto.UpdateExperimentRequest,
) (*proto.UpdateExperimentResponse, error) {
	editor, err := s.checkRole(ctx, accountproto.Account_EDITOR, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	if err := validateUpdateExperimentRequest(req); err != nil {
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
		experimentStorage := v2es.NewExperimentStorage(tx)
		experiment, err := experimentStorage.GetExperiment(ctx, req.Id, req.EnvironmentNamespace)
		if err != nil {
			return err
		}
		handler := command.NewExperimentCommandHandler(
			editor,
			experiment,
			s.publisher,
			req.EnvironmentNamespace,
		)
		if req.ChangeExperimentPeriodCommand != nil {
			if err = handler.Handle(ctx, req.ChangeExperimentPeriodCommand); err != nil {
				s.logger.Error(
					"Failed to change period",
					log.FieldsFromImcomingContext(ctx).AddFields(
						zap.Error(err),
						zap.String("environmentNamespace", req.EnvironmentNamespace),
					)...,
				)
				return err
			}
			return experimentStorage.UpdateExperiment(ctx, experiment, req.EnvironmentNamespace)
		}
		if req.ChangeNameCommand != nil {
			if err = handler.Handle(ctx, req.ChangeNameCommand); err != nil {
				s.logger.Error(
					"Failed to change Name",
					log.FieldsFromImcomingContext(ctx).AddFields(
						zap.Error(err),
						zap.String("environmentNamespace", req.EnvironmentNamespace),
					)...,
				)
				return err
			}
		}
		if req.ChangeDescriptionCommand != nil {
			if err = handler.Handle(ctx, req.ChangeDescriptionCommand); err != nil {
				s.logger.Error(
					"Failed to change Description",
					log.FieldsFromImcomingContext(ctx).AddFields(
						zap.Error(err),
						zap.String("environmentNamespace", req.EnvironmentNamespace),
					)...,
				)
				return err
			}
		}
		return experimentStorage.UpdateExperiment(ctx, experiment, req.EnvironmentNamespace)
	})
	if err != nil {
		if err == v2es.ErrExperimentNotFound || err == v2es.ErrExperimentUnexpectedAffectedRows {
			return nil, localizedError(statusNotFound, locale.JaJP)
		}
		s.logger.Error(
			"Failed to update experiment",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
			)...,
		)
		return nil, localizedError(statusInternal, locale.JaJP)
	}
	return &proto.UpdateExperimentResponse{}, nil
}

func validateUpdateExperimentRequest(req *proto.UpdateExperimentRequest) error {
	if req.Id == "" {
		return localizedError(statusExperimentIDRequired, locale.JaJP)
	}
	if req.ChangeExperimentPeriodCommand != nil {
		if err := validateExperimentPeriod(
			req.ChangeExperimentPeriodCommand.StartAt,
			req.ChangeExperimentPeriodCommand.StopAt,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *experimentService) StartExperiment(
	ctx context.Context,
	req *proto.StartExperimentRequest,
) (*proto.StartExperimentResponse, error) {
	editor, err := s.checkRole(ctx, accountproto.Account_EDITOR, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	if err := validateStartExperimentRequest(req); err != nil {
		return nil, err
	}
	if err := s.updateExperiment(ctx, editor, req.Command, req.Id, req.EnvironmentNamespace); err != nil {
		return nil, err
	}
	return &proto.StartExperimentResponse{}, nil
}

func validateStartExperimentRequest(req *proto.StartExperimentRequest) error {
	if req.Id == "" {
		return localizedError(statusExperimentIDRequired, locale.JaJP)
	}
	if req.Command == nil {
		return localizedError(statusNoCommand, locale.JaJP)
	}
	return nil
}

func (s *experimentService) FinishExperiment(
	ctx context.Context,
	req *proto.FinishExperimentRequest,
) (*proto.FinishExperimentResponse, error) {
	editor, err := s.checkRole(ctx, accountproto.Account_EDITOR, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	if err := validateFinishExperimentRequest(req); err != nil {
		return nil, err
	}
	if err := s.updateExperiment(ctx, editor, req.Command, req.Id, req.EnvironmentNamespace); err != nil {
		return nil, err
	}
	return &proto.FinishExperimentResponse{}, nil
}

func validateFinishExperimentRequest(req *proto.FinishExperimentRequest) error {
	if req.Id == "" {
		return localizedError(statusExperimentIDRequired, locale.JaJP)
	}
	if req.Command == nil {
		return localizedError(statusNoCommand, locale.JaJP)
	}
	return nil
}

func (s *experimentService) StopExperiment(
	ctx context.Context,
	req *proto.StopExperimentRequest,
) (*proto.StopExperimentResponse, error) {
	editor, err := s.checkRole(ctx, accountproto.Account_EDITOR, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	if err := validateStopExperimentRequest(req); err != nil {
		return nil, err
	}
	if err := s.updateExperiment(ctx, editor, req.Command, req.Id, req.EnvironmentNamespace); err != nil {
		return nil, err
	}
	return &proto.StopExperimentResponse{}, nil
}

func validateStopExperimentRequest(req *proto.StopExperimentRequest) error {
	if req.Id == "" {
		return localizedError(statusExperimentIDRequired, locale.JaJP)
	}
	if req.Command == nil {
		return localizedError(statusNoCommand, locale.JaJP)
	}
	return nil
}

func (s *experimentService) ArchiveExperiment(
	ctx context.Context,
	req *proto.ArchiveExperimentRequest,
) (*proto.ArchiveExperimentResponse, error) {
	editor, err := s.checkRole(ctx, accountproto.Account_EDITOR, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	if req.Id == "" {
		return nil, localizedError(statusExperimentIDRequired, locale.JaJP)
	}
	if req.Command == nil {
		return nil, localizedError(statusNoCommand, locale.JaJP)
	}
	err = s.updateExperiment(
		ctx,
		editor,
		req.Command,
		req.Id,
		req.EnvironmentNamespace,
	)
	if err != nil {
		s.logger.Error(
			"Failed to archive experiment",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", req.EnvironmentNamespace),
			)...,
		)
		return nil, err
	}
	return &proto.ArchiveExperimentResponse{}, nil
}

func (s *experimentService) DeleteExperiment(
	ctx context.Context,
	req *proto.DeleteExperimentRequest,
) (*proto.DeleteExperimentResponse, error) {
	editor, err := s.checkRole(ctx, accountproto.Account_EDITOR, req.EnvironmentNamespace)
	if err != nil {
		return nil, err
	}
	if err := validateDeleteExperimentRequest(req); err != nil {
		return nil, err
	}
	if err := s.updateExperiment(ctx, editor, req.Command, req.Id, req.EnvironmentNamespace); err != nil {
		return nil, err
	}
	return &proto.DeleteExperimentResponse{}, nil
}

func validateDeleteExperimentRequest(req *proto.DeleteExperimentRequest) error {
	if req.Id == "" {
		return localizedError(statusExperimentIDRequired, locale.JaJP)
	}
	if req.Command == nil {
		return localizedError(statusNoCommand, locale.JaJP)
	}
	return nil
}

func (s *experimentService) updateExperiment(
	ctx context.Context,
	editor *eventproto.Editor,
	cmd command.Command,
	id, environmentNamespace string,
) error {
	tx, err := s.mysqlClient.BeginTx(ctx)
	if err != nil {
		s.logger.Error(
			"Failed to begin transaction",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
			)...,
		)
		return localizedError(statusInternal, locale.JaJP)
	}
	err = s.mysqlClient.RunInTransaction(ctx, tx, func() error {
		experimentStorage := v2es.NewExperimentStorage(tx)
		experiment, err := experimentStorage.GetExperiment(ctx, id, environmentNamespace)
		if err != nil {
			s.logger.Error(
				"Failed to get experiment",
				log.FieldsFromImcomingContext(ctx).AddFields(
					zap.Error(err),
					zap.String("environmentNamespace", environmentNamespace),
				)...,
			)
			return err
		}
		handler := command.NewExperimentCommandHandler(editor, experiment, s.publisher, environmentNamespace)
		if err := handler.Handle(ctx, cmd); err != nil {
			s.logger.Error(
				"Failed to handle command",
				log.FieldsFromImcomingContext(ctx).AddFields(
					zap.Error(err),
					zap.String("environmentNamespace", environmentNamespace),
				)...,
			)
			return err
		}
		return experimentStorage.UpdateExperiment(ctx, experiment, environmentNamespace)
	})
	if err != nil {
		if err == v2es.ErrExperimentNotFound || err == v2es.ErrExperimentUnexpectedAffectedRows {
			return localizedError(statusNotFound, locale.JaJP)
		}
		s.logger.Error(
			"Failed to update experiment",
			log.FieldsFromImcomingContext(ctx).AddFields(
				zap.Error(err),
				zap.String("environmentNamespace", environmentNamespace),
			)...,
		)
		return localizedError(statusInternal, locale.JaJP)
	}
	return nil
}
