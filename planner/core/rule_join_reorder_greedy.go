// Copyright 2018 PingCAP, Inc.
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

package core

import (
	"math"
	"sort"

	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/parser/ast"
)

type joinReorderGreedySolver struct {
	*baseSingleGroupJoinOrderSolver
	eqEdges   []*expression.ScalarFunction
	joinTypes []JoinType
}

// solve reorders the join nodes in the group based on a greedy algorithm.
//
// For each node having a join equal condition with the current join tree in
// the group, calculate the cumulative join cost of that node and the join
// tree, choose the node with the smallest cumulative cost to join with the
// current join tree.
//
// cumulative join cost = CumCount(lhs) + CumCount(rhs) + RowCount(join)
//   For base node, its CumCount equals to the sum of the count of its subtree.
//   See baseNodeCumCost for more details.
// TODO: this formula can be changed to real physical cost in future.
//
// For the nodes and join trees which don't have a join equal condition to
// connect them, we make a bushy join tree to do the cartesian joins finally.
func (s *joinReorderGreedySolver) solve(joinNodePlans []LogicalPlan, tracer *joinReorderTrace) (LogicalPlan, error) {
	for _, node := range joinNodePlans {
		_, err := node.recursiveDeriveStats(nil)
		if err != nil {
			return nil, err
		}
		cost := s.baseNodeCumCost(node)
		s.curJoinGroup = append(s.curJoinGroup, &jrNode{
			p:       node,
			cumCost: cost,
		})
		tracer.appendLogicalJoinCost(node, cost)
	}

	// Sort plans by cost
	sort.SliceStable(s.curJoinGroup, func(i, j int) bool {
		return s.curJoinGroup[i].cumCost < s.curJoinGroup[j].cumCost
	})

	var cartesianGroup []LogicalPlan
	for len(s.curJoinGroup) > 0 {
		newNode, err := s.constructConnectedJoinTree(tracer)
		if err != nil {
			return nil, err
		}
		cartesianGroup = append(cartesianGroup, newNode.p)
	}

	return s.makeBushyJoin(cartesianGroup), nil
}

func (s *joinReorderGreedySolver) constructConnectedJoinTree(tracer *joinReorderTrace) (*jrNode, error) {
	curJoinTree := s.curJoinGroup[0]
	s.curJoinGroup = s.curJoinGroup[1:]
	for {
		bestCost := math.MaxFloat64
		bestIdx := -1
		var finalRemainOthers []expression.Expression
		var bestJoin LogicalPlan
		for i, node := range s.curJoinGroup {
			newJoin, remainOthers := s.checkConnectionAndMakeJoin(curJoinTree.p, node.p)
			if newJoin == nil {
				continue
			}
			_, err := newJoin.recursiveDeriveStats(nil)
			if err != nil {
				return nil, err
			}
			curCost := s.calcJoinCumCost(newJoin, curJoinTree, node)
			tracer.appendLogicalJoinCost(newJoin, curCost)
			if bestCost > curCost {
				bestCost = curCost
				bestJoin = newJoin
				bestIdx = i
				finalRemainOthers = remainOthers
			}
		}
		// If we could find more join node, meaning that the sub connected graph have been totally explored.
		if bestJoin == nil {
			break
		}
		curJoinTree = &jrNode{
			p:       bestJoin,
			cumCost: bestCost,
		}
		s.curJoinGroup = append(s.curJoinGroup[:bestIdx], s.curJoinGroup[bestIdx+1:]...)
		s.otherConds = finalRemainOthers
	}
	return curJoinTree, nil
}

func (s *joinReorderGreedySolver) checkConnectionAndMakeJoin(leftPlan, rightPlan LogicalPlan) (LogicalPlan, []expression.Expression) {
	var usedEdges []*expression.ScalarFunction
	remainOtherConds := make([]expression.Expression, len(s.otherConds))
	copy(remainOtherConds, s.otherConds)
	joinType := InnerJoin
	for idx, edge := range s.eqEdges {
		lCol := edge.GetArgs()[0].(*expression.Column)
		rCol := edge.GetArgs()[1].(*expression.Column)
		if leftPlan.Schema().Contains(lCol) && rightPlan.Schema().Contains(rCol) {
			joinType = s.joinTypes[idx]
			usedEdges = append(usedEdges, edge)
		} else if rightPlan.Schema().Contains(lCol) && leftPlan.Schema().Contains(rCol) {
			joinType = s.joinTypes[idx]
			if joinType != InnerJoin {
				rightPlan, leftPlan = leftPlan, rightPlan
				usedEdges = append(usedEdges, edge)
			} else {
				newSf := expression.NewFunctionInternal(s.ctx, ast.EQ, edge.GetType(), rCol, lCol).(*expression.ScalarFunction)
				usedEdges = append(usedEdges, newSf)
			}
		}
	}
	if len(usedEdges) == 0 {
		return nil, nil
	}
	var otherConds []expression.Expression
	var leftConds []expression.Expression
	var rightConds []expression.Expression
	mergedSchema := expression.MergeSchema(leftPlan.Schema(), rightPlan.Schema())

	remainOtherConds, leftConds = expression.FilterOutInPlace(remainOtherConds, func(expr expression.Expression) bool {
		return expression.ExprFromSchema(expr, leftPlan.Schema()) && !expression.ExprFromSchema(expr, rightPlan.Schema())
	})
	remainOtherConds, rightConds = expression.FilterOutInPlace(remainOtherConds, func(expr expression.Expression) bool {
		return expression.ExprFromSchema(expr, rightPlan.Schema()) && !expression.ExprFromSchema(expr, leftPlan.Schema())
	})
	remainOtherConds, otherConds = expression.FilterOutInPlace(remainOtherConds, func(expr expression.Expression) bool {
		return expression.ExprFromSchema(expr, mergedSchema)
	})
	return s.newJoinWithEdges(leftPlan, rightPlan, usedEdges, otherConds, leftConds, rightConds, joinType), remainOtherConds
}
