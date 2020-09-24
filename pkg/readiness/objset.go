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
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type objKey struct {
	gvk            schema.GroupVersionKind
	namespacedName types.NamespacedName
}

type objSet map[objKey]struct{}

func (k objKey) String() string {
	return fmt.Sprintf("%s [%s]", k.namespacedName.String(), k.gvk.String())

}
