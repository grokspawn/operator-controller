package source

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/operator-framework/operator-controller/api/operator-controller/v1"
)

func TestDynamicInformerSourceCloseBeforeStartErrors(t *testing.T) {
	dis := NewDynamicSource(DynamicSourceConfig{})
	require.Error(t, dis.Close(), "calling close before start should error")
}

func TestDynamicInformerSourceWaitForSyncTimeout(t *testing.T) {
	dis := NewDynamicSource(DynamicSourceConfig{})
	close(dis.startedChan)
	dis.informerCtx = context.Background()
	timeout, cancel := context.WithTimeout(context.TODO(), time.Millisecond*10)
	defer cancel()
	require.Error(t, dis.WaitForSync(timeout), "should error on timeout")
}

func TestDynamicInformerSourceWaitForSyncInformerContextClosed(t *testing.T) {
	dis := NewDynamicSource(DynamicSourceConfig{})
	close(dis.startedChan)
	timeout, cancel := context.WithTimeout(context.TODO(), time.Millisecond*10)
	defer cancel()
	dis.informerCtx = timeout
	require.Error(t, dis.WaitForSync(context.Background()), "should error on informer context closed")
}

func TestDynamicInformerSourceWaitForSyncErrorChannel(t *testing.T) {
	dis := NewDynamicSource(DynamicSourceConfig{})
	close(dis.startedChan)
	dis.informerCtx = context.Background()
	go func() {
		time.Sleep(time.Millisecond * 10)
		dis.err = errors.New("error")
		close(dis.erroredChan)
	}()
	require.Error(t, dis.WaitForSync(context.Background()), "should error on receiving error from channel")
}

func TestDynamicInformerSourceWaitForSyncAlreadyErrored(t *testing.T) {
	dis := NewDynamicSource(DynamicSourceConfig{})
	close(dis.startedChan)
	dis.informerCtx = context.Background()
	dis.err = errors.New("error")
	close(dis.erroredChan)
	require.Error(t, dis.WaitForSync(context.Background()), "should error since there is already a sync error")
}

func TestDynamicInformerSourceWaitForSyncAlreadySynced(t *testing.T) {
	dis := NewDynamicSource(DynamicSourceConfig{})
	close(dis.startedChan)
	close(dis.syncedChan)
	dis.informerCtx = context.Background()
	require.NoError(t, dis.WaitForSync(context.Background()), "should not error if already synced")
}

func TestDynamicInformerSourceWaitForSyncSyncedChannel(t *testing.T) {
	dis := NewDynamicSource(DynamicSourceConfig{})
	close(dis.startedChan)
	dis.informerCtx = context.Background()
	go func() {
		time.Sleep(time.Millisecond * 10)
		close(dis.syncedChan)
	}()
	require.NoError(t, dis.WaitForSync(context.Background()), "should not error on receiving struct from syncedChannel")
}

func TestDynamicInformerSourceWaitForSyncNotStarted(t *testing.T) {
	dis := NewDynamicSource(DynamicSourceConfig{})
	require.Error(t, dis.WaitForSync(context.Background()), "should error if not started")
}

func TestDynamicInformerSourceStartAlreadyStarted(t *testing.T) {
	dis := NewDynamicSource(DynamicSourceConfig{})
	close(dis.startedChan)
	require.Panics(t, func() { _ = dis.Start(context.Background(), nil) }, "should return an error if already started")
}

func TestDynamicInformerSourceStart(t *testing.T) {
	fakeDynamicClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme)
	infFact := dynamicinformer.NewDynamicSharedInformerFactory(fakeDynamicClient, time.Minute)
	dis := NewDynamicSource(DynamicSourceConfig{
		DynamicInformerFactory: infFact,
		GVR: schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "pods",
		},
		Owner:           &v1.ClusterExtension{},
		Handler:         handler.Funcs{},
		Predicates:      []predicate.Predicate{},
		OnPostSyncError: func(ctx context.Context) {},
	})

	require.NoError(t, dis.Start(context.Background(), nil))

	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, dis.WaitForSync(waitCtx))
	require.NoError(t, dis.Close())
}
