/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package types

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/protos/pb"
)

func toString(t *testing.T, values [][]Val, vID TypeID) []string {
	out := make([]string, len(values))
	for i, v := range values {
		b := ValueForType(StringID)
		require.NoError(t, Marshal(v[0], &b))
		out[i] = b.Value.(string)
	}
	return out
}

func getInput(t *testing.T, tid TypeID, in []string) [][]Val {
	list := make([][]Val, len(in))
	for i, s := range in {
		va := Val{StringID, []byte(s)}
		v, err := Convert(va, tid)
		require.NoError(t, err)
		list[i] = []Val{v}
	}
	return list
}

func getUIDList(n int) *pb.List {
	data := make([]uint64, 0, n)
	for i := 1; i <= n; i++ {
		data = append(data, uint64(i*100))
	}
	return &pb.List{Uids: data}
}

const charset = "abcdefghijklmnopqrstuvwxyz"

func StringWithCharset(length int) string {
	var seededRand *rand.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[seededRand.Intn(len(charset))]
	}
	return string(b)
}

func TestQuickSelect(t *testing.T) {
	n := 10000
	k := 10
	getList := func() [][]Val {
		strs := make([]string, n)
		for i := range n {
			strs[i] = fmt.Sprintf("%d", rand.Intn(100000))
		}

		list := make([][]Val, len(strs))
		for i, s := range strs {
			va := Val{StringID, []byte(s)}
			v, _ := Convert(va, IntID)
			list[i] = []Val{v}
		}

		return list
	}

	ul := getUIDList(n)
	list := getList()
	require.NoError(t, SortTopN(list, &ul.Uids, []bool{false}, "", k))

	for i := range k {
		for j := k; j < n; j++ {
			require.Equal(t, list[i][0].Value.(int64) <= list[j][0].Value.(int64), true)
		}
	}

}

func BenchmarkSortQuickSort(b *testing.B) {
	n := 1000000
	getList := func() [][]Val {
		strs := make([]string, n)
		for i := range n {
			strs[i] = StringWithCharset(10)
		}

		list := make([][]Val, len(strs))
		for i, s := range strs {
			va := Val{StringID, []byte(s)}
			v, _ := Convert(va, StringID)
			list[i] = []Val{v}
		}

		return list
	}

	b.Run(fmt.Sprintf("Normal Sort Ratio %d ", n), func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ul := getUIDList(n)
			list := getList()
			b.ResetTimer()
			err := Sort(list, &ul.Uids, []bool{false}, "")
			b.StopTimer()
			require.NoError(b, err)
		}
	})

	for j := 1; j < n; j += n / 6 {
		b.Run(fmt.Sprintf("QuickSort Sort Ratio %d %d", n, j), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				ul := getUIDList(n)
				list := getList()
				b.ResetTimer()
				err := SortTopN(list, &ul.Uids, []bool{false}, "", j)
				b.StopTimer()
				require.NoError(b, err)
			}
		})
	}
}

func TestSortStrings(t *testing.T) {
	list := getInput(t, StringID, []string{"bb", "aaa", "aa", "bab"})
	ul := getUIDList(4)
	require.NoError(t, Sort(list, &ul.Uids, []bool{false}, ""))
	require.EqualValues(t, []uint64{300, 200, 400, 100}, ul.Uids)
	require.EqualValues(t, []string{"aa", "aaa", "bab", "bb"},
		toString(t, list, StringID))
}

func TestSortLanguage(t *testing.T) {
	// Sorting strings of german language.
	list := getInput(t, StringID, []string{"öffnen", "zumachen"})
	ul := getUIDList(2)

	require.NoError(t, Sort(list, &ul.Uids, []bool{false}, "de"))
	require.EqualValues(t, []uint64{100, 200}, ul.Uids)
	require.EqualValues(t, []string{"öffnen", "zumachen"},
		toString(t, list, StringID))

	// Sorting strings of swedish language.
	list = getInput(t, StringID, []string{"öppna", "zon"})

	require.NoError(t, Sort(list, &ul.Uids, []bool{false}, "sv"))
	require.EqualValues(t, []uint64{200, 100}, ul.Uids)
	require.EqualValues(t, []string{"zon", "öppna"},
		toString(t, list, StringID))
}

func TestSortInts(t *testing.T) {
	list := getInput(t, IntID, []string{"22", "111", "11", "212"})
	ul := getUIDList(4)
	require.NoError(t, Sort(list, &ul.Uids, []bool{false}, ""))
	require.EqualValues(t, []uint64{300, 100, 200, 400}, ul.Uids)
	require.EqualValues(t, []string{"11", "22", "111", "212"},
		toString(t, list, IntID))
}

func TestSortFloats(t *testing.T) {
	list := getInput(t, FloatID, []string{"22.2", "11.2", "11.5", "2.12"})
	ul := getUIDList(4)
	require.NoError(t, Sort(list, &ul.Uids, []bool{false}, ""))
	require.EqualValues(t, []uint64{400, 200, 300, 100}, ul.Uids)
	require.EqualValues(t,
		[]string{"2.12", "11.2", "11.5", "22.2"},
		toString(t, list, FloatID))
}

func TestSortFloatsDesc(t *testing.T) {
	list := getInput(t, FloatID, []string{"22.2", "11.2", "11.5", "2.12"})
	ul := getUIDList(4)
	require.NoError(t, Sort(list, &ul.Uids, []bool{true}, ""))
	require.EqualValues(t, []uint64{100, 300, 200, 400}, ul.Uids)
	require.EqualValues(t,
		[]string{"22.2", "11.5", "11.2", "2.12"},
		toString(t, list, FloatID))
}

func TestSortDateTimes(t *testing.T) {
	in := []string{
		"2016-01-02T15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:06",
		"2006-01-02T15:04:01",
	}
	list := getInput(t, DateTimeID, in)
	ul := getUIDList(4)
	require.NoError(t, Sort(list, &ul.Uids, []bool{false}, ""))
	require.EqualValues(t, []uint64{400, 200, 300, 100}, ul.Uids)
	require.EqualValues(t,
		[]string{"2006-01-02T15:04:01Z", "2006-01-02T15:04:05Z",
			"2006-01-02T15:04:06Z", "2016-01-02T15:04:05Z"},
		toString(t, list, DateTimeID))
}

func TestSortIntAndFloat(t *testing.T) {
	list := [][]Val{
		{{Tid: IntID, Value: int64(55)}},
		{{Tid: FloatID, Value: 21.5}},
		{{Tid: IntID, Value: int64(100)}},
	}
	ul := getUIDList(3)
	require.NoError(t, Sort(list, &ul.Uids, []bool{false}, ""))
	require.EqualValues(t, []uint64{200, 100, 300}, ul.Uids)
	require.EqualValues(t,
		[]string{"21.5", "55", "100"},
		toString(t, list, DateTimeID))

}

func TestEqual(t *testing.T) {
	require.True(t, equal(Val{Tid: IntID, Value: int64(3)}, Val{Tid: IntID, Value: int64(3)}),
		"equal should return true for two equal values")

	require.False(t, equal(Val{Tid: IntID, Value: int64(3)}, Val{Tid: IntID, Value: int64(4)}),
		"equal should return false for two different values")

	// not equal when the types are different
	require.False(t, equal(Val{Tid: IntID, Value: int64(3)}, Val{Tid: FloatID, Value: float64(3.0)}),
		"equal should return false for two values with different types")

	// not equal when either parameter has the Value field being nil
	require.False(t, equal(Val{Tid: IntID, Value: int64(3)}, Val{Tid: IntID}),
		"equal should return false when either parameter cannot have its value converted")
	require.False(t, equal(Val{Tid: IntID}, Val{Tid: IntID, Value: int64(3)}),
		"equal should return false when either parameter cannot have its value converted")
	require.False(t, equal(Val{Tid: IntID}, Val{Tid: IntID}),
		"equal should return false when either parameter cannot have its value converted")

	// not equal when there is a type mismatch between value and tid for either parameter
	require.False(t, equal(Val{Tid: IntID, Value: float64(3.0)}, Val{Tid: FloatID, Value: float64(3.0)}),
		"equal should return false when either parameter's value has a type mismatch with its Tid")
	require.False(t, equal(Val{Tid: FloatID, Value: float64(3.0)}, Val{Tid: IntID, Value: float64(3.0)}),
		"equal should return false when either parameter's value has a type mismatch with its Tid")

}

func findIndex(t *testing.T, uids []uint64, uid uint64) int {
	for i := range uids {
		if uids[i] == uid {
			return i
		}
	}
	t.Errorf("Could not find index")
	return -1
}

func TestSortMismatchedTypes(t *testing.T) {
	list := [][]Val{
		{{Tid: StringID, Value: "cat"}},
		{{Tid: IntID, Value: int64(55)}},
		{{Tid: BoolID, Value: true}},
		{{Tid: FloatID, Value: 21.5}},
		{{Tid: StringID, Value: "aardvark"}},
		{{Tid: StringID, Value: "buffalo"}},
		{{Tid: FloatID, Value: 33.33}},
	}
	ul := getUIDList(7)
	require.NoError(t, Sort(list, &ul.Uids, []bool{false}, ""))

	// Don't care about relative ordering between types. However, like types
	// should be sorted with each other.
	catIdx := findIndex(t, ul.Uids, 100)
	aarIdx := findIndex(t, ul.Uids, 500)
	bufIdx := findIndex(t, ul.Uids, 600)
	require.True(t, aarIdx < bufIdx)
	require.True(t, bufIdx < catIdx)

	idx55 := findIndex(t, ul.Uids, 200)
	idx21 := findIndex(t, ul.Uids, 400)
	idx33 := findIndex(t, ul.Uids, 700)
	require.True(t, idx21 < idx33)
	require.True(t, idx33 < idx55)
}
