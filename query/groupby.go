/*
 * Copyright (C) 2017 Dgraph Labs, Inc. and Contributors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package query

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/dgraph-io/dgraph/algo"
	"github.com/dgraph-io/dgraph/protos/taskp"
	"github.com/dgraph-io/dgraph/types"
	"github.com/dgraph-io/dgraph/x"
)

type groupPair struct {
	key  types.Val
	attr string
}

type groupResult struct {
	keys       []groupPair
	aggregates []groupPair
	uids       []uint64
}

type groupResults struct {
	group []*groupResult
}

type groupElements struct {
	entities *taskp.List
	key      types.Val
}

type dedup struct {
	groups []uniq
}

type uniq struct {
	elements map[string]groupElements
	attr     string
}

func aggregateGroup(grp *groupResult, child *SubGraph) (types.Val, error) {
	ag := aggregator{
		name: child.SrcFunc[0],
	}
	for _, uid := range grp.uids {
		idx := sort.Search(len(child.SrcUIDs.Uids), func(i int) bool {
			return child.SrcUIDs.Uids[i] >= uid
		})
		if idx == len(child.SrcUIDs.Uids) || child.SrcUIDs.Uids[idx] != uid {
			continue
		}
		v := child.values[idx]
		val, err := convertWithBestEffort(v, child.Attr)
		if err != nil {
			continue
		}
		ag.Apply(val)
	}
	return ag.Value()
}

func (d *dedup) addValue(attr string, value types.Val, uid uint64) {
	idx := -1
	// Going last to first as we'll always add new ones to last and
	// would access it till we add a new entry.
	for i := len(d.groups) - 1; i >= 0; i-- {
		it := d.groups[i].attr
		if attr == it {
			idx = i
			break
		}
	}
	if idx == -1 {
		// Create a new entry.
		d.groups = append(d.groups, uniq{
			attr:     attr,
			elements: make(map[string]groupElements),
		})
		idx = len(d.groups) - 1
	}

	// Create the string key.
	var strKey string
	if value.Tid == types.UidID {
		strKey = strconv.FormatUint(uid, 10)
	} else {
		valC := types.Val{types.StringID, ""}
		err := types.Marshal(value, &valC)
		if err != nil {
			return
		}
		strKey = valC.Value.(string)
	}

	if _, ok := d.groups[idx].elements[strKey]; !ok {
		// If this is the first element of the group.
		d.groups[idx].elements[strKey] = groupElements{
			key:      value,
			entities: &taskp.List{make([]uint64, 0)},
		}
	}
	cur := d.groups[idx].elements[strKey].entities
	cur.Uids = append(cur.Uids, uid)
}

// formGroup creates all possible groups with the list of uids that belong to that
// group.
func (res *groupResults) formGroups(dedupMap dedup, cur *taskp.List, groupVal []groupPair) {
	l := len(groupVal)
	if l == len(dedupMap.groups) {
		a := make([]uint64, len(cur.Uids))
		b := make([]groupPair, len(groupVal))
		copy(a, cur.Uids)
		copy(b, groupVal)
		res.group = append(res.group, &groupResult{
			uids: a,
			keys: b,
		})
		return
	}

	for _, v := range dedupMap.groups[l].elements {
		temp := new(taskp.List)
		groupVal = append(groupVal, groupPair{
			key:  v.key,
			attr: dedupMap.groups[l].attr,
		})
		if l != 0 {
			algo.IntersectWith(cur, v.entities, temp)
		} else {
			temp.Uids = make([]uint64, len(v.entities.Uids))
			copy(temp.Uids, v.entities.Uids)
		}
		res.formGroups(dedupMap, temp, groupVal)
		groupVal = groupVal[:len(groupVal)-1]
	}
}

func processGroupBy(sg *SubGraph) error {
	mp := make(map[string]groupResult)
	_ = mp
	var dedupMap dedup
	idx := 0
	for _, child := range sg.Children {
		if !child.Params.ignoreResult {
			continue
		}
		if len(child.DestUIDs.Uids) != 0 {
			// It's a UID node.
			for i := 0; i < len(child.uidMatrix); i++ {
				srcUid := child.SrcUIDs.Uids[i]
				ul := child.uidMatrix[i]
				for _, uid := range ul.Uids {
					dedupMap.addValue(child.Attr, types.Val{Tid: types.UidID, Value: uid}, srcUid)
				}
			}
		} else {
			// It's a value node.
			for i, v := range child.values {
				srcUid := child.SrcUIDs.Uids[i]
				val, err := convertTo(v)
				if err != nil {
					continue
				}
				dedupMap.addValue(child.Attr, val, srcUid)
			}
		}
		idx++
	}

	// Create all the groups here.
	res := new(groupResults)
	res.formGroups(dedupMap, &taskp.List{}, []groupPair{})

	// Go over the groups and aggregate the values.
	for i := range res.group {
		grp := res.group[i]
		for _, child := range sg.Children {
			if child.Params.ignoreResult {
				continue
			}
			// This is a aggregation node.
			grp.aggregateChild(child)
		}
	}
	// Note: This is expensive. But done to keep the result order deterministic
	sort.Slice(res.group, func(i, j int) bool {
		return groupLess(res.group[i], res.group[j])
	})
	sg.GroupbyRes = res
	return nil
}

func (grp *groupResult) aggregateChild(child *SubGraph) {
	if child.Params.DoCount {
		(*grp).aggregates = append((*grp).aggregates, groupPair{
			attr: "count",
			key: types.Val{
				Tid:   types.IntID,
				Value: int64(len(grp.uids)),
			},
		})
	} else if len(child.SrcFunc) > 0 && isAggregatorFn(child.SrcFunc[0]) {
		fieldName := fmt.Sprintf("%s(%s)", child.SrcFunc[0], child.Attr)
		finalVal, err := aggregateGroup(grp, child)
		if err != nil {
			return
		}
		(*grp).aggregates = append((*grp).aggregates, groupPair{
			attr: fieldName,
			key:  finalVal,
		})
	}
}

func groupLess(a, b *groupResult) bool {
	if len(a.keys) < len(b.keys) {
		return true
	} else if len(a.keys) != len(b.keys) {
		return false
	}
	if len(a.aggregates) < len(b.aggregates) {
		return true
	} else if len(a.aggregates) != len(b.aggregates) {
		return false
	}
	if len(a.uids) < len(b.uids) {
		return true
	} else if len(a.uids) != len(b.uids) {
		return false
	}

	for i := range a.keys {
		l, err := types.Less(a.keys[i].key, b.keys[i].key)
		if err == nil {
			if l {
				return l
			}
			l, _ = types.Less(b.keys[i].key, a.keys[i].key)
			if l {
				return !l
			}
		}
	}

	for i := range a.aggregates {
		if l, err := types.Less(a.aggregates[i].key, b.aggregates[i].key); err == nil {
			if l {
				return l
			}
			l, _ = types.Less(b.aggregates[i].key, a.aggregates[i].key)
			if l {
				return !l
			}
		}
	}
	var asum, bsum uint64
	for i := range a.uids {
		asum += a.uids[i]
		bsum += b.uids[i]
		if a.uids[i] < b.uids[i] {
			return true
		} else if a.uids[i] > b.uids[i] {
			return false
		}
	}
	if asum < bsum {
		return true
	} else if asum != bsum {
		return false
	}
	x.Fatalf("wrong groups")
	return false
}
