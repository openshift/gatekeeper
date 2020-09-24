/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package readiness

import (
	"strconv"
	"testing"

	"github.com/onsi/gomega"
	"github.com/open-policy-agent/frameworks/constraint/pkg/apis/templates/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func makeCT(name string) *v1beta1.ConstraintTemplate {
	return &v1beta1.ConstraintTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func makeCTSlice(prefix string, count int) []runtime.Object {
	out := make([]runtime.Object, count)
	for i := 0; i < count; i++ {
		out[i] = makeCT(prefix + strconv.Itoa(i))
	}
	return out
}

// Verify that an unpopulated tracker is unsatisfied.
func Test_ObjectTracker_Unpopulated_Is_Unsatisfied(t *testing.T) {
	g := gomega.NewWithT(t)
	ot := newObjTracker(schema.GroupVersionKind{})
	g.Expect(ot.Satisfied()).NotTo(gomega.BeTrue(), "unpopulated tracker should not be satisfied")
}

// Verify that an populated tracker with no expectations is satisfied.
func Test_ObjectTracker_No_Expectations(t *testing.T) {
	g := gomega.NewWithT(t)
	ot := newObjTracker(schema.GroupVersionKind{})
	ot.ExpectationsDone()
	g.Expect(ot.Satisfied()).To(gomega.BeTrue(), "populated tracker with no expectations should be satisfied")
}

// Verify that that multiple expectations are tracked correctly.
func Test_ObjectTracker_Multiple_Expectations(t *testing.T) {
	g := gomega.NewWithT(t)
	ot := newObjTracker(schema.GroupVersionKind{})

	const count = 10
	ct := makeCTSlice("ct-", count)
	for i := 0; i < len(ct); i++ {
		ot.Expect(ct[i])
	}
	g.Expect(ot.Satisfied()).NotTo(gomega.BeTrue(), "should not be satisfied before ExpectationsDone")
	ot.ExpectationsDone()

	for i := 0; i < len(ct); i++ {
		g.Expect(ot.Satisfied()).NotTo(gomega.BeTrue(), "should not be satisfied before observations are done")
		ot.Observe(ct[i])
	}
	g.Expect(ot.Satisfied()).To(gomega.BeTrue(), "should be satisfied")
}

// Verify that observations can precede expectations.
func Test_ObjectTracker_Seen_Before_Expect(t *testing.T) {
	g := gomega.NewWithT(t)
	ot := newObjTracker(schema.GroupVersionKind{})
	ct := makeCT("test-ct")
	ot.Observe(ct)
	g.Expect(ot.Satisfied()).NotTo(gomega.BeTrue(), "unpopulated tracker should not be satisfied")
	ot.Expect(ct)

	ot.ExpectationsDone()
	g.Expect(ot.Satisfied()).To(gomega.BeTrue(), "should have been satisfied")
}

// Verify that terminated resources are ignored when calling Expect.
func Test_ObjectTracker_Terminated_Expect(t *testing.T) {
	g := gomega.NewWithT(t)
	ot := newObjTracker(schema.GroupVersionKind{})
	ct := makeCT("test-ct")
	now := metav1.Now()
	ct.ObjectMeta.DeletionTimestamp = &now
	ot.Expect(ct)
	ot.ExpectationsDone()
	g.Expect(ot.Satisfied()).To(gomega.BeTrue(), "should be satisfied")
}

// Verify that that expectations can be cancelled.
func Test_ObjectTracker_Cancelled_Expectations(t *testing.T) {
	g := gomega.NewWithT(t)
	ot := newObjTracker(schema.GroupVersionKind{})

	const count = 10
	ct := makeCTSlice("ct-", count)
	for i := 0; i < len(ct); i++ {
		ot.Expect(ct[i])
	}
	g.Expect(ot.Satisfied()).NotTo(gomega.BeTrue(), "should not be satisfied before ExpectationsDone")
	ot.ExpectationsDone()

	// Skip the first two
	for i := 2; i < len(ct); i++ {
		g.Expect(ot.Satisfied()).NotTo(gomega.BeTrue(), "should not be satisfied before observations are done")
		ot.Observe(ct[i])
	}
	g.Expect(ot.Satisfied()).NotTo(gomega.BeTrue(), "two expectation remain")

	ot.CancelExpect(ct[0])
	g.Expect(ot.Satisfied()).NotTo(gomega.BeTrue(), "one expectation remains")

	ot.CancelExpect(ct[1])
	g.Expect(ot.Satisfied()).To(gomega.BeTrue(), "should be satisfied")
}

// Verify that that duplicate expectations only need a single observation.
func Test_ObjectTracker_Duplicate_Expectations(t *testing.T) {
	g := gomega.NewWithT(t)
	ot := newObjTracker(schema.GroupVersionKind{})

	const count = 10
	ct := makeCTSlice("ct-", count)
	for i := 0; i < len(ct); i++ {
		ot.Expect(ct[i])
		ot.Expect(ct[i])
	}
	g.Expect(ot.Satisfied()).NotTo(gomega.BeTrue(), "should not be satisfied before ExpectationsDone")
	ot.ExpectationsDone()

	for i := 0; i < len(ct); i++ {
		g.Expect(ot.Satisfied()).NotTo(gomega.BeTrue(), "should not be satisfied before observations are done")
		ot.Observe(ct[i])
	}
	g.Expect(ot.Satisfied()).To(gomega.BeTrue(), "should be satisfied")
}

// Verify that an expectation can be canceled before it's first expected.
func Test_ObjectTracker_CancelBeforeExpect(t *testing.T) {
	g := gomega.NewWithT(t)
	ot := newObjTracker(schema.GroupVersionKind{})
	ct := makeCT("test-ct")
	ot.CancelExpect(ct)
	ot.Expect(ct)
	ot.ExpectationsDone()

	g.Expect(ot.Satisfied()).To(gomega.BeTrue())
}

// Verify that the allSatisfied circuit breaker keeps Satisfied==true and
// no other operations have any impact.
func Test_ObjectTracker_CircuitBreaker(t *testing.T) {
	g := gomega.NewWithT(t)
	ot := newObjTracker(schema.GroupVersionKind{})

	const count = 10
	ct := makeCTSlice("ct-", count)
	for i := 0; i < len(ct); i++ {
		ot.Expect(ct[i])
	}

	g.Expect(ot.Satisfied()).NotTo(gomega.BeTrue(), "should not be satisfied before ExpectationsDone")
	ot.ExpectationsDone()

	for i := 0; i < len(ct); i++ {
		ot.Observe(ct[i])
	}

	g.Expect(ot.Satisfied()).To(gomega.BeTrue())

	// The circuit-breaker should have been tripped. Let's try different operations
	// and make sure they do not consume additional memory or affect the satisfied state.
	for i := 0; i < len(ct); i++ {
		ot.CancelExpect(ct[i])
		ot.Observe(ct[i])
	}

	expectNoObserve := makeCTSlice("expectNoObserve-", count)
	for i := 0; i < len(expectNoObserve); i++ {
		// Expect resources we won't then observe
		ot.Expect(expectNoObserve[i])
	}
	cancelNoObserve := makeCTSlice("cancelNoObserve-", count)
	for i := 0; i < len(cancelNoObserve); i++ {
		// Cancel resources we won't then observe
		ot.CancelExpect(cancelNoObserve[i])
	}
	justObserve := makeCTSlice("justObserve-", count)
	for i := 0; i < len(justObserve); i++ {
		// Observe some unexpected resources
		ot.Observe(justObserve[i])
	}

	g.Expect(ot.Satisfied()).To(gomega.BeTrue())

	// Peek at internals - we should not be accruing memory from the post-circuit-breaker operations
	ot.mu.RLock()
	defer ot.mu.RUnlock()
	g.Expect(ot.cancelled).To(gomega.BeEmpty())
	g.Expect(ot.expect).To(gomega.BeEmpty())
	g.Expect(ot.seen).To(gomega.BeEmpty())
	g.Expect(ot.satisfied).To(gomega.BeEmpty())
}

// Verifies the kinds internal method and that it retains it values even after
// the circuit-breaker trips (which releases memory it depends on).
func Test_ObjectTracker_kinds(t *testing.T) {
	g := gomega.NewWithT(t)
	ot := newObjTracker(schema.GroupVersionKind{})

	const count = 10
	ct := makeCTSlice("ct-", count)
	for i := 0; i < len(ct); i++ {
		ot.Expect(ct[i])
	}
	ot.ExpectationsDone()

	kindsBefore := ot.kinds()

	for i := 0; i < len(ct); i++ {
		ot.Observe(ct[i])
	}

	g.Expect(ot.Satisfied()).Should(gomega.BeTrue(), "should be satisfied")

	kindsAfter := ot.kinds()

	g.Expect(kindsBefore).ShouldNot(gomega.BeEmpty(), "expected non-empty kinds")
	g.Expect(kindsAfter).Should(gomega.Equal(kindsBefore), "expected kinds to match")
}
