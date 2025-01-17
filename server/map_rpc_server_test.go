// Copyright 2017 Google Inc. All Rights Reserved.
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

package server

import (
	"context"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/golang/protobuf/proto"
	"github.com/google/trillian"
	"github.com/google/trillian/extension"
	"github.com/google/trillian/storage"
	stestonly "github.com/google/trillian/storage/testonly"
	"github.com/kylelemons/godebug/pretty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const mapID1 = int64(1)

func TestIsHealthy(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		desc          string
		accessibleErr error
	}{
		{"healthy", nil},
		{"unhealthy", errors.New("DB not happy")},
	}

	opts := TrillianMapServerOptions{}

	for _, test := range tests {
		fakeStorage := storage.NewMockMapStorage(ctrl)
		fakeStorage.EXPECT().CheckDatabaseAccessible(gomock.Any()).Return(test.accessibleErr)

		server := NewTrillianMapServer(extension.Registry{
			AdminStorage: fakeAdminStorageForMap(ctrl, 1, mapID1),
			MapStorage:   fakeStorage,
		}, opts)

		wantErr := test.accessibleErr != nil
		err := server.IsHealthy()
		if gotErr := err != nil; gotErr != wantErr {
			t.Errorf("%s: IsHealthy() err? %t want? %t (err=%v)", test.desc, gotErr, wantErr, err)
		}
	}
}

func TestInitMap(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		desc       string
		getRootErr error
		wantInit   bool
		root       []byte
		wantCode   codes.Code
	}{
		{desc: "init new map", getRootErr: storage.ErrTreeNeedsInit, wantInit: true, root: nil, wantCode: codes.OK},
		{desc: "init new map, no err", getRootErr: nil, wantInit: true, root: nil, wantCode: codes.OK},
		{desc: "init already initialised map", getRootErr: nil, wantInit: false, root: []byte{}, wantCode: codes.AlreadyExists},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockTX := storage.NewMockMapTreeTX(ctrl)
			fakeStorage := &stestonly.FakeMapStorage{TX: mockTX}
			if tc.getRootErr != nil {
				mockTX.EXPECT().LatestSignedMapRoot(gomock.Any()).Return(nil, tc.getRootErr)
			} else {
				mockTX.EXPECT().LatestSignedMapRoot(gomock.Any()).Return(
					&trillian.SignedMapRoot{MapRoot: tc.root}, nil)
			}

			mockTX.EXPECT().IsOpen().AnyTimes().Return(false)
			mockTX.EXPECT().Close().Return(nil)
			if tc.wantInit {
				mockTX.EXPECT().Commit(gomock.Any()).Return(nil)
				mockTX.EXPECT().StoreSignedMapRoot(gomock.Any(), gomock.Any())
			}

			server := NewTrillianMapServer(extension.Registry{
				AdminStorage: fakeAdminStorageForMap(ctrl, 2, mapID1),
				MapStorage:   fakeStorage,
			}, TrillianMapServerOptions{})

			c, err := server.InitMap(ctx, &trillian.InitMapRequest{
				MapId: mapID1,
			})
			if got, want := status.Code(err), tc.wantCode; got != want {
				t.Errorf("InitMap returned %v, want %v", got, want)
			}
			if tc.wantInit {
				if err != nil {
					t.Fatalf("InitLog returned %v, want no error", err)
				}
				if c.Created == nil {
					t.Error("InitLog first attempt didn't return the created STH.")
				}
			} else {
				if err == nil {
					t.Errorf("InitLog returned nil, want error")
				}
			}
		})
	}
}

func TestGetSignedMapRoot_NotInitialised(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx := context.Background()

	fakeStorage := storage.NewMockMapStorage(ctrl)
	fakeAdmin := storage.NewMockAdminStorage(ctrl)
	mockAdminTX := storage.NewMockAdminTX(ctrl)
	mockAdminTX.EXPECT().GetTree(gomock.Any(), int64(12345)).Return(&trillian.Tree{TreeType: trillian.TreeType_MAP, TreeId: 12345}, nil)
	mockAdminTX.EXPECT().Close().Return(nil)
	mockAdminTX.EXPECT().Commit().Return(nil)
	fakeAdmin.EXPECT().Snapshot(gomock.Any()).Return(mockAdminTX, nil)
	mockTX := storage.NewMockMapTreeTX(ctrl)
	server := NewTrillianMapServer(extension.Registry{
		MapStorage:   fakeStorage,
		AdminStorage: fakeAdmin,
	}, TrillianMapServerOptions{})
	fakeStorage.EXPECT().SnapshotForTree(gomock.Any(), gomock.Any()).Return(mockTX, nil)
	mockTX.EXPECT().LatestSignedMapRoot(gomock.Any()).Return(nil, storage.ErrTreeNeedsInit)
	mockTX.EXPECT().Close()

	smrResp, err := server.GetSignedMapRoot(ctx, &trillian.GetSignedMapRootRequest{MapId: 12345})

	if err != storage.ErrTreeNeedsInit {
		t.Errorf("GetSignedMapRoot()=%v, nil want ErrTreeNeedsInit", err)
	}
	if smrResp != nil {
		t.Errorf("GetSignedMapRoot()=%v, _ want nil", smrResp)
	}
}

func TestGetSignedMapRoot(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx := context.Background()

	tests := []struct {
		desc               string
		req                *trillian.GetSignedMapRootRequest
		mapRoot            *trillian.SignedMapRoot
		snapShErr, lsmrErr error
	}{
		{
			desc:    "Map is empty, head at revision 0",
			req:     &trillian.GetSignedMapRootRequest{MapId: mapID1},
			mapRoot: &trillian.SignedMapRoot{Signature: []byte("notempty")},
		},
		{
			desc:    "Map has leaves, head > revision 0",
			req:     &trillian.GetSignedMapRootRequest{MapId: mapID1},
			mapRoot: &trillian.SignedMapRoot{Signature: []byte("notempty2")},
		},
		{
			desc:    "LatestSignedMapRoot returns error",
			req:     &trillian.GetSignedMapRootRequest{MapId: mapID1},
			lsmrErr: errors.New("sql: no rows in result set"),
		},
		{
			desc:      "Snapshot returns Error",
			req:       &trillian.GetSignedMapRootRequest{MapId: mapID1},
			snapShErr: errors.New("unknown map"),
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			adminStorage := fakeAdminStorageForMap(ctrl, 2, mapID1)
			fakeStorage := storage.NewMockMapStorage(ctrl)
			mockTX := storage.NewMockMapTreeTX(ctrl)

			// Calls from GetSignedMapRoot()
			fakeStorage.EXPECT().SnapshotForTree(gomock.Any(), gomock.Any()).Return(mockTX, test.snapShErr)
			if test.snapShErr == nil {
				mockTX.EXPECT().LatestSignedMapRoot(gomock.Any()).Return(test.mapRoot, test.lsmrErr)
				if test.lsmrErr == nil {
					mockTX.EXPECT().Commit(gomock.Any()).Return(nil)
				}
				mockTX.EXPECT().IsOpen().AnyTimes().Return(false)
			}
			mockTX.EXPECT().Close().Return(nil)

			server := NewTrillianMapServer(extension.Registry{
				AdminStorage: adminStorage,
				MapStorage:   fakeStorage,
			}, TrillianMapServerOptions{})

			smrResp, err := server.GetSignedMapRoot(ctx, test.req)

			wantErr := test.snapShErr != nil || test.lsmrErr != nil
			if gotErr := err != nil; gotErr != wantErr {
				t.Errorf("GetSignedMapRoot()=_, err? %t want? %t (err=%v)", gotErr, wantErr, err)
			}
			if err != nil {
				return
			}
			want := &trillian.GetSignedMapRootResponse{MapRoot: test.mapRoot}
			if got := smrResp; !proto.Equal(got, want) {
				diff := pretty.Compare(got, want)
				t.Errorf("GetSignedMapRoot() got != want, diff:\n%v", diff)
			}
		})
	}
}

func TestGetSignedMapRootByRevision_NotInitialised(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx := context.Background()

	fakeStorage := storage.NewMockMapStorage(ctrl)
	adminStorage := fakeAdminStorageForMap(ctrl, 2, 12345)
	mockTX := storage.NewMockMapTreeTX(ctrl)
	server := NewTrillianMapServer(extension.Registry{
		MapStorage:   fakeStorage,
		AdminStorage: adminStorage,
	}, TrillianMapServerOptions{})
	fakeStorage.EXPECT().SnapshotForTree(gomock.Any(), gomock.Any()).Return(mockTX, nil)
	mockTX.EXPECT().GetSignedMapRoot(gomock.Any(), gomock.Any()).Return(nil, storage.ErrTreeNeedsInit)
	mockTX.EXPECT().Close()

	smrResp, err := server.GetSignedMapRootByRevision(ctx, &trillian.GetSignedMapRootByRevisionRequest{
		MapId:    12345,
		Revision: 1,
	})

	if err != storage.ErrTreeNeedsInit {
		t.Errorf("GetSignedMapRootByRevision()=%v, nil want ErrTreeNeedsInit", err)
	}
	if smrResp != nil {
		t.Errorf("GetSignedMapRootByRevision()=%v, _ want nil", smrResp)
	}
}

func TestGetSignedMapRootByRevision(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		desc               string
		req                *trillian.GetSignedMapRootByRevisionRequest
		mapRoot            *trillian.SignedMapRoot
		snapShErr, lsmrErr error
		wantErr            bool
	}{
		{
			desc:    "Request revision 0 for empty map",
			req:     &trillian.GetSignedMapRootByRevisionRequest{MapId: mapID1},
			lsmrErr: errors.New("sql: no rows in result set"),
			wantErr: true,
		},
		{
			desc:    "Request invalid -ve revision",
			req:     &trillian.GetSignedMapRootByRevisionRequest{MapId: mapID1, Revision: -1},
			wantErr: true,
		},
		{
			desc:    "Request future revision (123) for empty map",
			req:     &trillian.GetSignedMapRootByRevisionRequest{MapId: mapID1, Revision: 123},
			lsmrErr: errors.New("sql: no rows in result set"),
			wantErr: true,
		},
		{
			desc: "Request revision >0 for non-empty map",
			req:  &trillian.GetSignedMapRootByRevisionRequest{MapId: mapID1, Revision: 1},
			mapRoot: &trillian.SignedMapRoot{
				Signature: []byte("0F\002!\000\307b\255\223\353\23615&\022\263\323\341\342+\276\274$\rX?\366\014U\362\006\376\0269rcm\002!\000\241*\255\220\301\263D\033\275\374\340A\377\337\354\202\331%au\3179\000O\r9\237\302\021\r\363\263"),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			adminStorage := fakeAdminStorageForMap(ctrl, 1, mapID1)
			fakeStorage := storage.NewMockMapStorage(ctrl)
			mockTX := storage.NewMockMapTreeTX(ctrl)

			if !test.wantErr || !(test.lsmrErr == nil && test.snapShErr == nil) {
				fakeStorage.EXPECT().SnapshotForTree(gomock.Any(), gomock.Any()).Return(mockTX, test.snapShErr)
				if test.snapShErr == nil {
					mockTX.EXPECT().GetSignedMapRoot(gomock.Any(), test.req.Revision).Return(test.mapRoot, test.lsmrErr)
					if test.lsmrErr == nil {
						mockTX.EXPECT().Commit(gomock.Any()).Return(nil)
					}
					mockTX.EXPECT().Close().Return(nil)
					mockTX.EXPECT().IsOpen().AnyTimes().Return(false)
				}
			}

			server := NewTrillianMapServer(extension.Registry{
				AdminStorage: adminStorage,
				MapStorage:   fakeStorage,
			}, TrillianMapServerOptions{})

			smrResp, err := server.GetSignedMapRootByRevision(ctx, test.req)

			if gotErr := err != nil; gotErr != test.wantErr {
				t.Errorf("GetSignedMapRootByRevision()=_, err? %t want? %t (err=%v)", gotErr, test.wantErr, err)
			}
			if err != nil {
				return
			}
			want := &trillian.GetSignedMapRootResponse{MapRoot: test.mapRoot}
			if got := smrResp; !proto.Equal(got, want) {
				diff := pretty.Compare(got, want)
				t.Errorf("GetSignedMapRootByRevision() got != want, diff:\n%v", diff)
			}
		})
	}
}

func fakeAdminStorageForMap(ctrl *gomock.Controller, times int, treeID int64) storage.AdminStorage {
	tree := proto.Clone(stestonly.MapTree).(*trillian.Tree)
	tree.TreeId = treeID

	adminTX := storage.NewMockReadOnlyAdminTX(ctrl)
	adminStorage := &stestonly.FakeAdminStorage{
		ReadOnlyTX: []storage.ReadOnlyAdminTX{adminTX},
	}

	adminTX.EXPECT().GetTree(gomock.Any(), treeID).MaxTimes(times).Return(tree, nil)
	adminTX.EXPECT().Close().MaxTimes(times).Return(nil)
	adminTX.EXPECT().Commit().MaxTimes(times).Return(nil)

	return adminStorage
}

func TestRequestIndexValidator(t *testing.T) {
	tests := []struct {
		desc      string
		indexSize int
		indices   [][]byte
		wantErr   bool
	}{
		{
			desc:      "Single index of correct length",
			indexSize: 1,
			indices:   [][]byte{{'a'}},
		},
		{
			desc:      "Single index of longer correct length",
			indexSize: 4,
			indices:   [][]byte{{'a', 'b', 'c', 'd'}},
		},
		{
			desc:      "Single index too long",
			indexSize: 1,
			indices:   [][]byte{{'a', 'b'}},
			wantErr:   true,
		},
		{
			desc:      "Single index too short",
			indexSize: 2,
			indices:   [][]byte{{'a'}},
			wantErr:   true,
		},
		{
			desc:      "Multiple indices of correct length & no duplicates",
			indexSize: 1,
			indices:   [][]byte{{'a'}, {'b'}},
		},
		{
			desc:      "Multiple indices of correct length with duplicates",
			indexSize: 1,
			indices:   [][]byte{{'a'}, {'a'}},
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			err := validateIndices(tt.indexSize, len(tt.indices), func(i int) []byte { return tt.indices[i] })

			if (err != nil) != tt.wantErr {
				t.Errorf("validateIndices() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
