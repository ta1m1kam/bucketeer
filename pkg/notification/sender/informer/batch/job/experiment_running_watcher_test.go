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

package job

import (
	"context"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	environmentclientmock "github.com/bucketeer-io/bucketeer/pkg/environment/client/mock"
	experimentclientmock "github.com/bucketeer-io/bucketeer/pkg/experiment/client/mock"
	sendermock "github.com/bucketeer-io/bucketeer/pkg/notification/sender/mock"
	environmentproto "github.com/bucketeer-io/bucketeer/proto/environment"
	experimentproto "github.com/bucketeer-io/bucketeer/proto/experiment"
)

func TestCreateExperimentRunningNotification(t *testing.T) {
	t.Parallel()
	mockController := gomock.NewController(t)
	defer mockController.Finish()

	patterns := map[string]struct {
		setup       func(*testing.T, *ExperimentRunningWatcher)
		expectedErr error
	}{
		"no experiment": {
			setup: func(t *testing.T, w *ExperimentRunningWatcher) {
				w.environmentClient.(*environmentclientmock.MockClient).EXPECT().ListEnvironments(
					gomock.Any(), gomock.Any()).Return(
					&environmentproto.ListEnvironmentsResponse{
						Environments: []*environmentproto.Environment{{Id: "ns0", Namespace: "ns0"}},
						Cursor:       "",
					}, nil)
				w.experimentClient.(*experimentclientmock.MockClient).EXPECT().ListExperiments(
					gomock.Any(), gomock.Any()).Return(
					&experimentproto.ListExperimentsResponse{
						Experiments: []*experimentproto.Experiment{},
					}, nil)
			},
		},
		"experiments exist": {
			setup: func(t *testing.T, w *ExperimentRunningWatcher) {
				w.environmentClient.(*environmentclientmock.MockClient).EXPECT().ListEnvironments(
					gomock.Any(), gomock.Any()).Return(
					&environmentproto.ListEnvironmentsResponse{
						Environments: []*environmentproto.Environment{{Id: "ns0", Namespace: "ns0"}},
						Cursor:       "",
					}, nil)
				w.experimentClient.(*experimentclientmock.MockClient).EXPECT().ListExperiments(
					gomock.Any(), gomock.Any()).Return(
					&experimentproto.ListExperimentsResponse{
						Experiments: []*experimentproto.Experiment{{
							Id:   "eid",
							Name: "ename",
						}, {
							Id:   "eid1",
							Name: "ename1",
						}},
					}, nil)
				w.sender.(*sendermock.MockSender).EXPECT().Send(gomock.Any(), gomock.Any()).Return(nil).Times(1)
			},
			expectedErr: nil,
		},
	}
	for msg, p := range patterns {
		t.Run(msg, func(t *testing.T) {
			w := newExperimentRunningWatcherWithMock(t, mockController)
			if p.setup != nil {
				p.setup(t, w)
			}
			err := w.Run(context.Background())
			assert.Equal(t, p.expectedErr, err)
		})
	}
}

func newExperimentRunningWatcherWithMock(t *testing.T, c *gomock.Controller) *ExperimentRunningWatcher {
	t.Helper()
	return &ExperimentRunningWatcher{
		environmentClient: environmentclientmock.NewMockClient(c),
		experimentClient:  experimentclientmock.NewMockClient(c),
		sender:            sendermock.NewMockSender(c),
		logger:            zap.NewNop(),
		opts: &options{
			timeout: 5 * time.Minute,
		},
	}
}
