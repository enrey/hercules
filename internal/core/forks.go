package core

import (
	"log"
	"reflect"
	"sort"

	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/hercules.v4/internal/toposort"
		)

// OneShotMergeProcessor provides the convenience method to consume merges only once.
type OneShotMergeProcessor struct {
	merges map[plumbing.Hash]bool
}

// Initialize resets OneShotMergeProcessor.
func (proc *OneShotMergeProcessor) Initialize() {
	proc.merges = map[plumbing.Hash]bool{}
}

// ShouldConsumeCommit returns true on regular commits. It also returns true upon
// the first occurrence of a particular merge commit.
func (proc *OneShotMergeProcessor) ShouldConsumeCommit(deps map[string]interface{}) bool {
	commit := deps[DependencyCommit].(*object.Commit)
	if commit.NumParents() <= 1 {
		return true
	}
	if !proc.merges[commit.Hash] {
		proc.merges[commit.Hash] = true
		return true
	}
	return false
}

// IsMergeCommit indicates whether the commit is a merge or not.
func IsMergeCommit(deps map[string]interface{}) bool {
	return deps[DependencyCommit].(*object.Commit).NumParents() > 1
}

// NoopMerger provides an empty Merge() method suitable for PipelineItem.
type NoopMerger struct {
}

// Merge does nothing.
func (merger *NoopMerger) Merge(branches []PipelineItem) {
	// no-op
}

// ForkSamePipelineItem clones items by referencing the same origin.
func ForkSamePipelineItem(origin PipelineItem, n int) []PipelineItem {
	clones := make([]PipelineItem, n)
	for i := 0; i < n; i++ {
		clones[i] = origin
	}
	return clones
}

// ForkCopyPipelineItem clones items by copying them by value from the origin.
func ForkCopyPipelineItem(origin PipelineItem, n int) []PipelineItem {
	originValue := reflect.Indirect(reflect.ValueOf(origin))
	originType := originValue.Type()
	clones := make([]PipelineItem, n)
	for i := 0; i < n; i++ {
		cloneValue := reflect.New(originType).Elem()
		cloneValue.Set(originValue)
		clones[i] = cloneValue.Addr().Interface().(PipelineItem)
	}
	return clones
}

const (
	// runActionCommit corresponds to a regular commit
	runActionCommit = 0
	// runActionFork splits a branch into several parts
	runActionFork = iota
	// runActionMerge merges several branches together
	runActionMerge = iota
	// runActionDelete removes the branch as it is no longer needed
	runActionDelete = iota
)

type runAction struct {
	Action int
	Commit *object.Commit
	Items []int
}

type orderer = func(reverse, direction bool) []string

func cloneItems(origin []PipelineItem, n int) [][]PipelineItem {
	clones := make([][]PipelineItem, n)
	for j := 0; j < n; j++ {
		clones[j] = make([]PipelineItem, len(origin))
	}
	for i, item := range origin {
		itemClones := item.Fork(n)
		for j := 0; j < n; j++ {
			clones[j][i] = itemClones[j]
		}
	}
	return clones
}

func mergeItems(branches [][]PipelineItem) {
	buffer := make([]PipelineItem, len(branches) - 1)
	for i, item := range branches[0] {
		for j := 0; j < len(branches)-1; j++ {
			buffer[j] = branches[j+1][i]
		}
		item.Merge(buffer)
	}
}

// getMasterBranch returns the branch with the smallest index.
func getMasterBranch(branches map[int][]PipelineItem) []PipelineItem {
	minKey := 1 << 31
	var minVal []PipelineItem
	for key, val := range branches {
		if key < minKey {
			minKey = key
			minVal = val
		}
	}
	return minVal
}

// prepareRunPlan schedules the actions for Pipeline.Run().
func prepareRunPlan(commits []*object.Commit) []runAction {
	hashes, dag := buildDag(commits)
	leaveRootComponent(hashes, dag)
	numParents := bindNumParents(hashes, dag)
	mergedDag, mergedSeq := mergeDag(hashes, dag)
	orderNodes := bindOrderNodes(mergedDag)
	collapseFastForwards(orderNodes, hashes, mergedDag, dag, mergedSeq)
	/*fmt.Printf("digraph Hercules {\n")
	for i, c := range orderNodes(false, false) {
		commit := hashes[c]
		fmt.Printf("  \"%s\"[label=\"[%d] %s\"]\n", commit.Hash.String(), i, commit.Hash.String()[:6])
		for _, child := range mergedDag[commit.Hash] {
			fmt.Printf("  \"%s\" -> \"%s\"\n", commit.Hash.String(), child.Hash.String())
		}
	}
	fmt.Printf("}\n")*/
	plan := generatePlan(orderNodes, numParents, hashes, mergedDag, dag, mergedSeq)
	plan = optimizePlan(plan)
	/*for _, p := range plan {
		firstItem := p.Items[0]
		switch p.Action {
		case runActionCommit:
			fmt.Fprintln(os.Stderr, "C", firstItem, p.Commit.Hash.String())
		case runActionFork:
			fmt.Fprintln(os.Stderr, "F", p.Items)
		case runActionMerge:
			fmt.Fprintln(os.Stderr, "M", p.Items)
		}
	}*/
	return plan
}

// buildDag generates the raw commit DAG and the commit hash map.
func buildDag(commits []*object.Commit) (
	map[string]*object.Commit, map[plumbing.Hash][]*object.Commit) {

	hashes := map[string]*object.Commit{}
	for _, commit := range commits {
		hashes[commit.Hash.String()] = commit
	}
	dag := map[plumbing.Hash][]*object.Commit{}
	for _, commit := range commits {
		if _, exists := dag[commit.Hash]; !exists {
			dag[commit.Hash] = make([]*object.Commit, 0, 1)
		}
		for _, parent := range commit.ParentHashes {
			if _, exists := hashes[parent.String()]; !exists {
				continue
			}
			children := dag[parent]
			if children == nil {
				children = make([]*object.Commit, 0, 1)
			}
			dag[parent] = append(children, commit)
		}
	}
	return hashes, dag
}

// bindNumParents returns curried "numParents" function.
func bindNumParents(
	hashes map[string]*object.Commit,
	dag map[plumbing.Hash][]*object.Commit) func(c *object.Commit) int {
	return func(c *object.Commit) int {
		r := 0
		for _, parent := range c.ParentHashes {
			if p, exists := hashes[parent.String()]; exists {
				for _, pc := range dag[p.Hash] {
					if pc.Hash == c.Hash {
						r++
						break
					}
				}
			}
		}
		return r
	}
}

// leaveRootComponent runs connected components analysis and throws away everything
// but the part which grows from the root.
func leaveRootComponent(
	hashes map[string]*object.Commit,
	dag map[plumbing.Hash][]*object.Commit) {

	visited := map[plumbing.Hash]bool{}
	var sets [][]plumbing.Hash
	for key := range dag {
		if visited[key] {
			continue
		}
		var set []plumbing.Hash
		for queue := []plumbing.Hash{key}; len(queue) > 0; {
			head := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if visited[head] {
				continue
			}
			set = append(set, head)
			visited[head] = true
			for _, c := range dag[head] {
				if !visited[c.Hash] {
					queue = append(queue, c.Hash)
				}
			}
			if commit, exists := hashes[head.String()]; exists {
				for _, p := range commit.ParentHashes {
					if !visited[p] {
						if _, exists := hashes[p.String()]; exists {
							queue = append(queue, p)
						}
					}
				}
			}
		}
		sets = append(sets, set)
	}
	if len(sets) > 1 {
		maxlen := 0
		maxind := -1
		for i, set := range sets {
			if len(set) > maxlen {
				maxlen = len(set)
				maxind = i
			}
		}
		for i, set := range sets {
			if i == maxind {
				continue
			}
			for _, h := range set {
				log.Printf("warning: dropped %s from the analysis - disjoint", h.String())
				delete(dag, h)
				delete(hashes, h.String())
			}
		}
	}
}

// bindOrderNodes returns curried "orderNodes" function.
func bindOrderNodes(mergedDag map[plumbing.Hash][]*object.Commit) orderer {
	return func(reverse, direction bool) []string {
		graph := toposort.NewGraph()
		keys := make([]plumbing.Hash, 0, len(mergedDag))
		for key := range mergedDag {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, key := range keys {
			graph.AddNode(key.String())
		}
		for _, key := range keys {
			children := mergedDag[key]
			sort.Slice(children, func(i, j int) bool {
				return children[i].Hash.String() < children[j].Hash.String()
			})
			for _, c := range children {
				if !direction {
					graph.AddEdge(key.String(), c.Hash.String())
				} else {
					graph.AddEdge(c.Hash.String(), key.String())
				}
			}
		}
		order, ok := graph.Toposort()
		if !ok {
			// should never happen
			panic("Could not topologically sort the DAG of commits")
		}
		if reverse != direction {
			// one day this must appear in the standard library...
			for i, j := 0, len(order)-1; i < len(order)/2; i, j = i+1, j-1 {
				order[i], order[j] = order[j], order[i]
			}
		}
		return order
	}
}

// mergeDag turns sequences of consecutive commits into single nodes.
func mergeDag(
	hashes map[string]*object.Commit,
	dag map[plumbing.Hash][]*object.Commit) (
	mergedDag, mergedSeq map[plumbing.Hash][]*object.Commit) {

	parents := map[plumbing.Hash][]plumbing.Hash{}
	for key, vals := range dag {
		for _, val := range vals {
			parents[val.Hash] = append(parents[val.Hash], key)
		}
	}
	mergedDag = map[plumbing.Hash][]*object.Commit{}
	mergedSeq = map[plumbing.Hash][]*object.Commit{}
	visited := map[plumbing.Hash]bool{}
	for head := range dag {
		if visited[head] {
			continue
		}
		c := head
		for true {
			next := parents[c]
			if len(next) != 1 || len(dag[next[0]]) != 1 {
				break
			}
			c = next[0]
		}
		head = c
		var seq []*object.Commit
		for true {
			visited[c] = true
			seq = append(seq, hashes[c.String()])
			if len(dag[c]) != 1 {
				break
			}
			c = dag[c][0].Hash
			if len(parents[c]) != 1 {
				break
			}
		}
		mergedSeq[head] = seq
		mergedDag[head] = dag[seq[len(seq)-1].Hash]
	}
	return
}

// collapseFastForwards removes the fast forward merges.
func collapseFastForwards(
	orderNodes orderer, hashes map[string]*object.Commit,
	mergedDag, dag, mergedSeq map[plumbing.Hash][]*object.Commit)  {

	parents := map[plumbing.Hash][]plumbing.Hash{}
	for key, vals := range mergedDag {
		for _, val := range vals {
			parents[val.Hash] = append(parents[val.Hash], key)
		}
	}
	processed := map[plumbing.Hash]bool{}
	for _, strkey := range orderNodes(false, true) {
		key := hashes[strkey].Hash
		processed[key] = true
		repeat:
		vals, exists := mergedDag[key]
		if !exists {
			continue
		}
		if len(vals) < 2 {
			continue
		}
		toRemove := map[plumbing.Hash]bool{}
		for _, child := range vals {
			var queue []plumbing.Hash
			visited := map[plumbing.Hash]bool{child.Hash: true}
			childParents := parents[child.Hash]
			childNumOtherParents := 0
			for _, parent := range childParents {
				if parent != key {
					visited[parent] = true
					childNumOtherParents++
					queue = append(queue, parent)
				}
			}
			var immediateParent plumbing.Hash
			if childNumOtherParents == 1 {
				immediateParent = queue[0]
			}
			for len(queue) > 0 {
				head := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				if processed[head] {
					if head == key {
						toRemove[child.Hash] = true
						if childNumOtherParents == 1 && len(mergedDag[immediateParent]) == 1 {
							mergedSeq[immediateParent] = append(
								mergedSeq[immediateParent], mergedSeq[child.Hash]...)
							delete(mergedSeq, child.Hash)
							mergedDag[immediateParent] = mergedDag[child.Hash]
							delete(mergedDag, child.Hash)
							parents[child.Hash] = parents[immediateParent]
							for _, vals := range parents {
								for i, v := range vals {
									if v == child.Hash {
										vals[i] = immediateParent
										break
									}
								}
							}
						}
					}
					break
				}
				for _, parent := range parents[head] {
					if !visited[parent] {
						visited[head] = true
						queue = append(queue, parent)
					}
				}
			}
		}
		if len(toRemove) == 0 {
			continue
		}
		var newVals []*object.Commit
		for _, child := range vals {
			if !toRemove[child.Hash] {
				newVals = append(newVals, child)
			}
		}
		merged := false
		if len(newVals) == 1 {
			onlyChild := newVals[0].Hash
			if len(parents[onlyChild]) == 1 {
				merged = true
				mergedSeq[key] = append(mergedSeq[key], mergedSeq[onlyChild]...)
				delete(mergedSeq, onlyChild)
				mergedDag[key] = mergedDag[onlyChild]
				delete(mergedDag, onlyChild)
				parents[onlyChild] = parents[key]
				for _, vals := range parents {
					for i, v := range vals {
						if v == onlyChild {
							vals[i] = key
							break
						}
					}
				}
			}
		}
		if !merged {
			mergedDag[key] = newVals
		}
		newVals = []*object.Commit{}
		node := mergedSeq[key][len(mergedSeq[key])-1].Hash
		for _, child := range dag[node] {
			if !toRemove[child.Hash] {
				newVals = append(newVals, child)
			}
		}
		dag[node] = newVals
		if merged {
			goto repeat
		}
	}
}

// generatePlan creates the list of actions from the commit DAG.
func generatePlan(
	orderNodes orderer, numParents func(c *object.Commit) int,
	hashes map[string]*object.Commit,
	mergedDag, dag, mergedSeq map[plumbing.Hash][]*object.Commit) []runAction {

	var plan []runAction
	branches := map[plumbing.Hash]int{}
	branchers := map[plumbing.Hash]map[plumbing.Hash]int{}
	counter := 1
	for seqIndex, name := range orderNodes(false, true) {
		commit := hashes[name]
		if seqIndex == 0 {
			branches[commit.Hash] = 0
		}
		var branch int
		{
			var exists bool
			branch, exists = branches[commit.Hash]
			if !exists {
				branch = -1
			}
		}
		branchExists := func() bool { return branch >= 0 }
		appendCommit := func(c *object.Commit, branch int) {
			plan = append(plan, runAction{
				Action: runActionCommit,
				Commit: c,
				Items: []int{branch},
			})

		}
		appendMergeIfNeeded := func() {
			if numParents(commit) < 2 {
				return
			}
			// merge after the merge commit (the first in the sequence)
			var items []int
			minBranch := 1 << 31
			for _, parent := range commit.ParentHashes {
				if _, exists := hashes[parent.String()]; !exists {
					continue
				}
				parentBranch := -1
				if parents, exists := branchers[commit.Hash]; exists {
					if inheritedBranch, exists := parents[parent]; exists {
						parentBranch = inheritedBranch
					}
				}
				if parentBranch == -1 {
					parentBranch = branches[parent]
				}
				if len(dag[parent]) == 1 && minBranch > parentBranch {
					minBranch = parentBranch
				}
				items = append(items, parentBranch)
				if parentBranch != branch {
					appendCommit(commit, parentBranch)
				}
			}
			if minBranch < 1 << 31 {
				branch = minBranch
				branches[commit.Hash] = minBranch
			} else if !branchExists() {
				log.Panicf("!branchExists(%s)", commit.Hash.String())
			}
			plan = append(plan, runAction{
				Action: runActionMerge,
				Commit: nil,
				Items: items,
			})
		}
		var head plumbing.Hash
		if subseq, exists := mergedSeq[commit.Hash]; exists {
			for subseqIndex, offspring := range subseq {
				if branchExists() {
					appendCommit(offspring, branch)
				}
				if subseqIndex == 0 {
					appendMergeIfNeeded()
				}
			}
			head = subseq[len(subseq)-1].Hash
			branches[head] = branch
		} else {
			head = commit.Hash
		}
		if len(mergedDag[commit.Hash]) > 1 {
			children := []int{branch}
			for i, child := range mergedDag[commit.Hash] {
				if i == 0 {
					branches[child.Hash] = branch
					continue
				}
				if _, exists := branches[child.Hash]; !exists {
					branches[child.Hash] = counter
				}
				parents := branchers[child.Hash]
				if parents == nil {
					parents = map[plumbing.Hash]int{}
					branchers[child.Hash] = parents
				}
				parents[head] = counter
				children = append(children, counter)
				counter++
			}
			plan = append(plan, runAction{
				Action: runActionFork,
				Commit: nil,
				Items:  children,
			})
		}
	}
	return plan
}

// optimizePlan removes "dead" nodes and inserts `runActionDelete` disposal steps.
//
// |   *
// *  /
// |\/
// |/
// *
//
func optimizePlan(plan []runAction) []runAction {
	// lives maps branch index to the number of commits in that branch
	lives := map[int]int{}
	// lastMentioned maps branch index to the index inside `plan` when that branch was last used
	lastMentioned := map[int]int{}
	for i, p := range plan {
		firstItem := p.Items[0]
		switch p.Action {
		case runActionCommit:
			lives[firstItem]++
			lastMentioned[firstItem] = i
		case runActionFork:
			lastMentioned[firstItem] = i
		case runActionMerge:
			for _, item := range p.Items {
				lastMentioned[item] = i
			}
		}
	}
	branchesToDelete := map[int]bool{}
	for key, life := range lives {
		if life == 1 {
			branchesToDelete[key] = true
			delete(lastMentioned, key)
		}
	}
	var optimizedPlan []runAction
	lastMentionedArr := make([][2]int, 0, len(lastMentioned) + 1)
	for key, val := range lastMentioned {
		if val != len(plan) - 1 {
			lastMentionedArr = append(lastMentionedArr, [2]int{val, key})
		}
	}
	if len(lastMentionedArr) == 0 && len(branchesToDelete) == 0 {
		// early return - we have nothing to optimize
		return plan
	}
	sort.Slice(lastMentionedArr, func(i, j int) bool {
		return lastMentionedArr[i][0] < lastMentionedArr[j][0]
	})
	lastMentionedArr = append(lastMentionedArr, [2]int{len(plan)-1, -1})
	prevpi := -1
	for _, pair := range lastMentionedArr {
		for pi := prevpi + 1; pi <= pair[0]; pi++ {
			p := plan[pi]
			switch p.Action {
			case runActionCommit:
				if !branchesToDelete[p.Items[0]] {
					optimizedPlan = append(optimizedPlan, p)
				}
			case runActionFork:
				var newBranches []int
				for _, b := range p.Items {
					if !branchesToDelete[b] {
						newBranches = append(newBranches, b)
					}
				}
				if len(newBranches) > 1 {
					optimizedPlan = append(optimizedPlan, runAction{
						Action: runActionFork,
						Commit: p.Commit,
						Items:  newBranches,
					})
				}
			case runActionMerge:
				var newBranches []int
				for _, b := range p.Items {
					if !branchesToDelete[b] {
						newBranches = append(newBranches, b)
					}
				}
				if len(newBranches) > 1 {
					optimizedPlan = append(optimizedPlan, runAction{
						Action: runActionMerge,
						Commit: p.Commit,
						Items:  newBranches,
					})
				}
			}
		}
		if pair[1] >= 0 {
			prevpi = pair[0]
			optimizedPlan = append(optimizedPlan, runAction{
				Action: runActionDelete,
				Commit: nil,
				Items:  []int{pair[1]},
			})
		}
	}
	// single commit can be detected as redundant
	if len(optimizedPlan) > 0 {
		return optimizedPlan
	}
	return plan
}