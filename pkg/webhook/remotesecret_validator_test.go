package webhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/redhat-appstudio/remote-secret/api/v1beta1"
)

func TestValidateCreate(t *testing.T) {
	v := &RemoteSecretValidator{}

	runner := func(rs *api.RemoteSecret) error {
		return v.ValidateCreate(context.TODO(), rs)
	}

	testUploadData(t, runner)

	testDataFrom(t, false, runner)

	testUniqueTargets(t, runner)
}

func TestValidateUpdate(t *testing.T) {
	v := &RemoteSecretValidator{}

	runner := func(rs *api.RemoteSecret) error {
		return v.ValidateUpdate(context.TODO(), nil, rs)
	}

	testUploadData(t, runner)

	testDataFrom(t, true, runner)

	testUniqueTargets(t, runner)
}

func TestValidateDelete(t *testing.T) {
	v := &RemoteSecretValidator{}
	assert.NoError(t, v.ValidateDelete(context.TODO(), nil))
}

func testDataFrom(t *testing.T, testDataPresent bool, op func(*api.RemoteSecret) error) {
	t.Run("DataFrom", func(t *testing.T) {
		rs := &api.RemoteSecret{
			DataFrom: &api.RemoteSecretDataFrom{
				Name: "somename",
			},
		}
		assert.NoError(t, op(rs))

		t.Run("with UploadData", func(t *testing.T) {
			rs := rs.DeepCopy()
			rs.UploadData = map[string]string{
				"a": "b",
			}
			assert.Error(t, op(rs))
		})

		if testDataPresent {
			t.Run("with data present", func(t *testing.T) {
				rs := rs.DeepCopy()
				rs.Status.Conditions = []metav1.Condition{}
				meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
					Type:   string(api.RemoteSecretConditionTypeDataObtained),
					Status: metav1.ConditionTrue,
				})
				assert.Error(t, op(rs))
			})
		}
	})
}

func testUploadData(t *testing.T, op func(*api.RemoteSecret) error) {
	t.Run("UploadData", func(t *testing.T) {
		rs := &api.RemoteSecret{
			UploadData: map[string]string{"a": "b"},
		}
		assert.NoError(t, op(rs))

		t.Run("with empty DataFrom", func(t *testing.T) {
			rs := rs.DeepCopy()
			rs.DataFrom = &api.RemoteSecretDataFrom{}
			assert.NoError(t, op(rs))
		})

		t.Run("with DataFrom", func(t *testing.T) {
			rs := rs.DeepCopy()
			rs.DataFrom = &api.RemoteSecretDataFrom{
				Name: "non-empty",
			}
			assert.Error(t, op(rs))
		})
	})
}

func testUniqueTargets(t *testing.T, op func(*api.RemoteSecret) error) {
	t.Run("unique targets", func(t *testing.T) {
		rs := &api.RemoteSecret{}
		t.Run("local cluster", func(t *testing.T) {
			rs := rs.DeepCopy()
			rs.Spec.Targets = []api.RemoteSecretTarget{
				{
					Namespace: "a",
				},
				{
					Namespace: "a",
				},
			}
			assert.Error(t, op(rs))
		})
		t.Run("remote cluster", func(t *testing.T) {
			rs := rs.DeepCopy()
			rs.Spec.Targets = []api.RemoteSecretTarget{
				{
					Namespace: "a",
					ApiUrl:    "over-there",
				},
				{
					Namespace: "a",
					ApiUrl:    "over-there",
				},
			}
			assert.Error(t, op(rs))
		})
	})
}
