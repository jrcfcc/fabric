/*
Copyright IBM Corp. 2016 All Rights Reserved.

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

package simplebft

import (
	"bytes"
	"fmt"
	"reflect"
)

func (s *SBFT) maybeSendNewView() {
	if s.lastNewViewSent != nil && s.lastNewViewSent.View == s.view {
		return
	}

	vset := make(map[uint64]*Signed)
	var vcs []*ViewChange

	for src, state := range s.replicaState {
		if state.viewchange != nil && state.viewchange.View == s.view {
			vset[uint64(src)] = state.signedViewchange
			vcs = append(vcs, state.viewchange)
		}
	}

	xset, newBatch, ok := s.makeXset(vcs)
	if !ok {
		log.Debug("xset not yet sufficient")
		return
	}

	var batch *Batch
	if newBatch != nil {
		batch = newBatch
	} else if reflect.DeepEqual(s.cur.subject.Digest, xset.Digest) {
		batch = s.cur.preprep.Batch
	} else {
		log.Warningf("forfeiting primary - do not have request in store for %d %x", xset.Seq.Seq, xset.Digest)
		xset = nil
	}

	nv := &NewView{
		View:  s.view,
		Vset:  vset,
		Xset:  xset,
		Batch: batch,
	}

	log.Noticef("sending new view for %d", nv.View)
	s.lastNewViewSent = nv
	s.broadcast(&Msg{&Msg_NewView{nv}})
}

func (s *SBFT) checkNewViewSignatures(nv *NewView) ([]*ViewChange, error) {
	var vcs []*ViewChange
	for vcsrc, svc := range nv.Vset {
		vc := &ViewChange{}
		err := s.checkSig(svc, vcsrc, vc)
		if err == nil {
			_, err = s.checkBatch(vc.Checkpoint, false, true)
			if vc.View != nv.View {
				err = fmt.Errorf("view does not match")
			}
		}
		if err != nil {
			return nil, fmt.Errorf("viewchange from %d: %s", vcsrc, err)
		}
		vcs = append(vcs, vc)
	}

	return vcs, nil
}

func (s *SBFT) handleNewView(nv *NewView, src uint64) {
	if src != s.primaryIDView(nv.View) {
		log.Warningf("invalid new view from %d for %d", src, nv.View)
		return
	}

	if onv := s.replicaState[s.primaryIDView(nv.View)].newview; onv != nil && onv.View >= nv.View {
		log.Debugf("discarding duplicate new view for %d", nv.View)
		return
	}

	vcs, err := s.checkNewViewSignatures(nv)
	if err != nil {
		log.Warningf("invalid new view from %d: %s", src, err)
		s.sendViewChange()
		return
	}

	if nv.Batch == nil {
		log.Warningf("invalid new view from %d: no batch attached", src)
		s.sendViewChange()
		return
	}

	xset, newBatch, ok := s.makeXset(vcs)

	if !ok || !reflect.DeepEqual(nv.Xset, xset) {
		log.Warningf("invalid new view from %d: xset incorrect: %v, %v", src, nv.Xset, xset)
		s.sendViewChange()
		return
	}

	if nv.Xset == nil {
		if !bytes.Equal(nv.Batch.Hash(), newBatch.Hash()) {
			log.Warningf("invalid new view from %d: new batch for null request does not match: %x, %x, %v",
				src, nv.Batch.Hash(), newBatch.Hash(), nv)
			s.sendViewChange()
			return
		}
	} else if !bytes.Equal(nv.Batch.Hash(), nv.Xset.Digest) {
		log.Warningf("invalid new view from %d: batch head hash does not match xset: %x, %x, %v",
			src, hash(nv.Batch.Header), nv.Xset.Digest, nv)
		s.sendViewChange()
		return
	}

	_, err = s.checkBatch(nv.Batch, true, false)
	if err != nil {
		log.Warningf("invalid new view from %d: invalid batch, %s",
			src, err)
		s.sendViewChange()
		return
	}

	s.replicaState[s.primaryIDView(nv.View)].newview = nv

	if nv.View > s.view {
		s.view = nv.View
		s.activeView = false
	}

	s.processNewView()
}

func (s *SBFT) processNewView() {
	if s.activeView {
		return
	}

	nv := s.replicaState[s.primaryIDView(s.view)].newview
	if nv == nil || nv.View != s.view {
		return
	}

	s.activeView = true
	s.discardBacklog(s.primaryID())

	pp := &Preprepare{
		Seq:   &SeqView{Seq: nv.Batch.DecodeHeader().Seq, View: s.view},
		Batch: nv.Batch,
	}

	s.handleCheckedPreprepare(pp)

	s.processBacklog()
}
