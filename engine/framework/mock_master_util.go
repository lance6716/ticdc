// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package framework

// This file provides helper function to let the implementation of MasterImpl
// can finish its unit tests.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/pingcap/tiflow/engine/pkg/client"
	"github.com/pingcap/tiflow/pkg/errors"
	"github.com/stretchr/testify/require"

	pb "github.com/pingcap/tiflow/engine/enginepb"
	"github.com/pingcap/tiflow/engine/framework/metadata"
	frameModel "github.com/pingcap/tiflow/engine/framework/model"
	"github.com/pingcap/tiflow/engine/framework/statusutil"
	"github.com/pingcap/tiflow/engine/model"
	"github.com/pingcap/tiflow/engine/pkg/clock"
	dcontext "github.com/pingcap/tiflow/engine/pkg/context"
	"github.com/pingcap/tiflow/engine/pkg/deps"
	resourcemeta "github.com/pingcap/tiflow/engine/pkg/externalresource/resourcemeta/model"
	metaMock "github.com/pingcap/tiflow/engine/pkg/meta/mock"
	pkgOrm "github.com/pingcap/tiflow/engine/pkg/orm"
	"github.com/pingcap/tiflow/engine/pkg/p2p"
	"github.com/pingcap/tiflow/pkg/uuid"
)

// MockBaseMaster returns a mock DefaultBaseMaster
func MockBaseMaster(t *testing.T, id frameModel.MasterID, masterImpl MasterImpl) *DefaultBaseMaster {
	ctx := dcontext.Background()
	dp := deps.NewDeps()
	cli, err := pkgOrm.NewMockClient()
	if err != nil {
		panic(err)
	}
	err = dp.Provide(func() masterParamListForTest {
		return masterParamListForTest{
			MessageHandlerManager: p2p.NewMockMessageHandlerManager(),
			MessageSender:         p2p.NewMockMessageSender(),
			FrameMetaClient:       cli,
			BusinessClientConn:    metaMock.NewMockClientConn(),
			ExecutorGroup:         client.NewMockExecutorGroup(),
			ServerMasterClient:    client.NewMockServerMasterClient(gomock.NewController(t)),
		}
	})
	if err != nil {
		panic(err)
	}

	ctx = ctx.WithDeps(dp)

	ret := NewBaseMaster(
		ctx,
		masterImpl,
		id,
		FakeTask,
	)

	return ret.(*DefaultBaseMaster)
}

// MockBaseMasterCreateWorker mocks to create worker in base master
func MockBaseMasterCreateWorker(
	t *testing.T,
	master *DefaultBaseMaster,
	workerType frameModel.WorkerType,
	config WorkerConfig,
	cost model.RescUnit,
	masterID frameModel.MasterID,
	workerID frameModel.WorkerID,
	executorID model.ExecutorID,
	resources []resourcemeta.ResourceID,
	workerEpoch frameModel.Epoch,
) {
	master.uuidGen = uuid.NewMock()
	expectedSchedulerReq := &pb.ScheduleTaskRequest{
		TaskId:               workerID,
		Cost:                 int64(cost),
		ResourceRequirements: resourcemeta.ToResourceRequirement(masterID, resources...),
	}
	master.serverMasterClient.(*client.MockServerMasterClient).EXPECT().
		ScheduleTask(gomock.Any(), gomock.Eq(expectedSchedulerReq)).
		Return(&pb.ScheduleTaskResponse{
			ExecutorId: string(executorID),
		}, nil).Times(1)

	mockExecutorClient := client.NewMockExecutorClient(gomock.NewController(t))
	master.executorGroup.(*client.MockExecutorGroup).AddClient(executorID, mockExecutorClient)
	configBytes, err := json.Marshal(config)
	require.NoError(t, err)

	mockExecutorClient.EXPECT().DispatchTask(gomock.Any(),
		gomock.Eq(&client.DispatchTaskArgs{
			WorkerID:     workerID,
			MasterID:     masterID,
			WorkerType:   int64(workerType),
			WorkerConfig: configBytes,
			WorkerEpoch:  workerEpoch,
		}), gomock.Any(), gomock.Any()).Do(
		func(
			ctx context.Context,
			args *client.DispatchTaskArgs,
			start client.StartWorkerCallback,
			abort client.AbortWorkerCallback,
		) {
			start()
		}).Times(1).Return(nil)

	master.uuidGen.(*uuid.MockGenerator).Push(workerID)
}

// MockBaseMasterCreateWorkerMetScheduleTaskError mocks ScheduleTask meets error
func MockBaseMasterCreateWorkerMetScheduleTaskError(
	t *testing.T,
	master *DefaultBaseMaster,
	workerType frameModel.WorkerType,
	config WorkerConfig,
	cost model.RescUnit,
	masterID frameModel.MasterID,
	workerID frameModel.WorkerID,
	executorID model.ExecutorID,
) {
	master.uuidGen = uuid.NewMock()
	expectedSchedulerReq := &pb.ScheduleTaskRequest{
		TaskId: workerID,
		Cost:   int64(cost),
	}
	master.serverMasterClient.(*client.MockServerMasterClient).EXPECT().
		ScheduleTask(gomock.Any(), gomock.Eq(expectedSchedulerReq)).
		Return(nil, errors.ErrClusterResourceNotEnough.FastGenByArgs()).
		Times(1)

	master.uuidGen.(*uuid.MockGenerator).Push(workerID)
}

// MockBaseMasterWorkerHeartbeat sends HeartbeatPingMessage with mock message handler
func MockBaseMasterWorkerHeartbeat(
	t *testing.T,
	master *DefaultBaseMaster,
	masterID frameModel.MasterID,
	workerID frameModel.WorkerID,
	executorID p2p.NodeID,
) {
	worker, ok := master.workerManager.GetWorkers()[workerID]
	require.True(t, ok)
	workerEpoch := worker.Status().Epoch
	err := master.messageHandlerManager.(*p2p.MockMessageHandlerManager).InvokeHandler(
		t,
		frameModel.HeartbeatPingTopic(masterID),
		executorID,
		&frameModel.HeartbeatPingMessage{
			SendTime:     clock.MonoNow(),
			FromWorkerID: workerID,
			Epoch:        master.currentEpoch.Load(),
			WorkerEpoch:  workerEpoch,
		})

	require.NoError(t, err)
}

// MockBaseMasterWorkerUpdateStatus mocks to store status in metastore and sends
// WorkerStatusMessage.
func MockBaseMasterWorkerUpdateStatus(
	ctx context.Context,
	t *testing.T,
	master *DefaultBaseMaster,
	masterID frameModel.MasterID,
	workerID frameModel.WorkerID,
	executorID p2p.NodeID,
	status *frameModel.WorkerStatus,
) {
	workerMetaClient := metadata.NewWorkerMetadataClient(masterID, master.frameMetaClient)
	err := workerMetaClient.Store(ctx, status)
	require.NoError(t, err)

	err = master.messageHandlerManager.(*p2p.MockMessageHandlerManager).InvokeHandler(
		t,
		statusutil.WorkerStatusTopic(masterID),
		executorID,
		&statusutil.WorkerStatusMessage{
			Worker:      workerID,
			MasterEpoch: master.currentEpoch.Load(),
			Status:      status,
		})
	require.NoError(t, err)
}
