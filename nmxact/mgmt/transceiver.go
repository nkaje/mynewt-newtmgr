/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package mgmt

import (
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/runtimeco/go-coap"
	log "github.com/sirupsen/logrus"

	"mynewt.apache.org/newtmgr/nmxact/nmcoap"
	"mynewt.apache.org/newtmgr/nmxact/nmp"
	"mynewt.apache.org/newtmgr/nmxact/nmxutil"
	"mynewt.apache.org/newtmgr/nmxact/omp"
	"mynewt.apache.org/newtmgr/nmxact/sesn"
    rt "runtime/debug"
)

type TxFn func(req []byte) error

type Transceiver struct {
	// Only for plain NMP; nil for OMP transceivers.
	nd *nmp.Dispatcher

	// Used for OMP and CoAP resource requests.
	od *omp.Dispatcher

	txFilterCb nmcoap.MsgFilter

	isTcp bool
	proto sesn.MgmtProto
	wg    sync.WaitGroup
}

func NewTransceiver(txFilterCb, rxFilterCb nmcoap.MsgFilter, isTcp bool,
	mgmtProto sesn.MgmtProto, logDepth int) (*Transceiver, error) {

	t := &Transceiver{
		txFilterCb: txFilterCb,
		isTcp:      isTcp,
		proto:      mgmtProto,
	}

	if mgmtProto == sesn.MGMT_PROTO_NMP {
		t.nd = nmp.NewDispatcher(logDepth)
	}

	od, err := omp.NewDispatcher(rxFilterCb, isTcp, logDepth)
	if err != nil {
		return nil, err
	}
	t.od = od

	return t, nil
}

func (t *Transceiver) txRxNmp(txCb TxFn, req *nmp.NmpMsg, mtu int,
	timeout time.Duration) (nmp.NmpRsp, error) {

	nl, err := t.nd.AddListener(req.Hdr.Seq)
	if err != nil {
		return nil, err
	}
	defer t.nd.RemoveListener(req.Hdr.Seq)

	b, err := nmp.EncodeNmpPlain(req)
	if err != nil {
		return nil, err
	}

	log.Debugf("Tx NMP request: %s", hex.Dump(b))
	if t.isTcp == false && len(b) > mtu {
		return nil, fmt.Errorf("Request too big")
	}
	frags := nmxutil.Fragment(b, mtu)
	for _, frag := range frags {
		if err := txCb(frag); err != nil {
			return nil, err
		}
	}

	// Now wait for NMP response.
	for {
		select {
		case err := <-nl.ErrChan:
			return nil, err
		case rsp := <-nl.RspChan:
			return rsp, nil
		case _, ok := <-nl.AfterTimeout(timeout):
			if ok {
				return nil, nmxutil.NewRspTimeoutError("NMP timeout")
			}
		}
	}
}

func (t *Transceiver) txRxOmp(txCb TxFn, req *nmp.NmpMsg, mtu int,
	timeout time.Duration) (nmp.NmpRsp, error) {

	nl, err := t.od.AddNmpListener(req.Hdr.Seq)
	if err != nil {
		return nil, err
	}
	defer t.od.RemoveNmpListener(req.Hdr.Seq)

	var b []byte
	if t.isTcp {
		b, err = omp.EncodeOmpTcp(t.txFilterCb, req)
	} else {
		b, err = omp.EncodeOmpDgram(t.txFilterCb, req)
	}
	if err != nil {
		return nil, err
	}

	log.Debugf("transceiver t.isTcp %t proto %d, duration %d", t.isTcp, t.proto, timeout)
	log.Debugf("MTU %d len %d", mtu, len(b))
	log.Debugf("Tx OMP request: %s", hex.Dump(b))

	if t.isTcp == false && len(b) > mtu {
		return nil, fmt.Errorf("Request too big")
	}
	frags := nmxutil.Fragment(b, mtu)
    //rt.PrintStack()
	log.Debugf("frags len %d", len(frags))
	for _, frag := range frags {
		if err := txCb(frag); err != nil {
			return nil, err
		} else {
			log.Debugf("txCb fag")
        }
	}
    log.Debugf("txCb done")

	// Now wait for NMP response.
	for {
		select {
		case err := <-nl.ErrChan:
			return nil, err
		case rsp := <-nl.RspChan:
			return rsp, nil
		case _, ok := <-nl.AfterTimeout(timeout):
			if ok {
				return nil, nmxutil.NewRspTimeoutError("NMP timeout")
			}
		}
	}
}

func (t *Transceiver) TxRxMgmt(txCb TxFn, req *nmp.NmpMsg, mtu int,
	timeout time.Duration) (nmp.NmpRsp, error) {

	if t.nd != nil {
		return t.txRxNmp(txCb, req, mtu, timeout)
	} else {
		return t.txRxOmp(txCb, req, mtu, timeout)
	}
}

func (t *Transceiver) TxCoap(txCb TxFn, req coap.Message, mtu int) error {
	b, err := nmcoap.Encode(req)
	if err != nil {
		return err
	}

	log.Debugf("tx CoAP request: %s", hex.Dump(b))
	frags := nmxutil.Fragment(b, mtu)
	for _, frag := range frags {
		if err := txCb(frag); err != nil {
			return err
		}
	}

	return nil
}

func (t *Transceiver) ListenCoap(
	mc nmcoap.MsgCriteria) (*nmcoap.Listener, error) {

	mc.Path = strings.TrimPrefix(mc.Path, "/")

	ol, err := t.od.AddCoapListener(mc)
	if err != nil {
		return nil, err
	}

	return ol, nil
}

func (t *Transceiver) StopListenCoap(mc nmcoap.MsgCriteria) {
	mc.Path = strings.TrimPrefix(mc.Path, "/")
	t.od.RemoveCoapListener(mc)
}

func (t *Transceiver) DispatchNmpRsp(data []byte) {
	if t.nd != nil {
		log.Debugf("rx nmp response: %s", hex.Dump(data))
		t.nd.Dispatch(data)
	} else {
		log.Debugf("rx omp response: %s", hex.Dump(data))
        rt.PrintStack()
		t.od.Dispatch(data)
	}
}

func (t *Transceiver) DispatchCoap(data []byte) {
	t.od.Dispatch(data)
}

func (t *Transceiver) ProcessCoapReq(data []byte) (coap.Message, error) {
	return t.od.ProcessCoapReq(data)
}

func (t *Transceiver) ErrorOne(seq uint8, err error) {
	if t.nd != nil {
		t.nd.ErrorOne(seq, err)
	} else {
		t.od.ErrorOneNmp(seq, err)
	}
}

func (t *Transceiver) ErrorAll(err error) {
	if t.nd != nil {
		t.nd.ErrorAll(err)
	}
	t.od.ErrorAll(err)
}

func (t *Transceiver) AbortRx(seq uint8) {
	t.ErrorOne(seq, fmt.Errorf("rx aborted"))
}

func (t *Transceiver) Stop() {
	t.od.Stop()
}

func (t *Transceiver) MgmtProto() sesn.MgmtProto {
	return t.proto
}

func (t *Transceiver) Filters() (nmcoap.MsgFilter, nmcoap.MsgFilter) {
	return t.txFilterCb, t.od.RxFilter()
}

func (t *Transceiver) SetFilters(txFilter nmcoap.MsgFilter,
	rxFilter nmcoap.MsgFilter) {

	t.txFilterCb = txFilter
	t.od.SetRxFilter(rxFilter)
}
