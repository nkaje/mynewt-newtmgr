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

package xact

import (
	"mynewt.apache.org/newtmgr/nmxact/nmp"
	"mynewt.apache.org/newtmgr/nmxact/sesn"
	log "github.com/sirupsen/logrus"
)

func txReq(s sesn.Sesn, m *nmp.NmpMsg, c *CmdBase) (
	nmp.NmpRsp, error) {

	if c.abortErr != nil {
		return nil, c.abortErr
	}

	c.curNmpSeq = m.Hdr.Seq
	c.curSesn = s
	defer func() {
		c.curNmpSeq = 0
		c.curSesn = nil
	}()

    log.Debugf("txReq TxRxMgmt sesn %v seq %d", c.curSesn, c.curNmpSeq)
	rsp, err := sesn.TxRxMgmt(s, m, c.TxOptions())
	if err != nil {
        log.Debugf("**Error**: txReq TxRxMgmt sesn %v seq %d", c.curSesn, c.curNmpSeq)
		return nil, err
	}

	return rsp, nil
}
/* 
func txReq_async(s sesn.Sesn, m *nmp.NmpMsg, c *CmdBase) (
       nmp.NmpRsp, error) {
        rsp_chan := make(chan nmp.Rsp)

       if c.abortErr != nil {
               return nil, c.abortErr
       }

       c.curNmpSeq = m.Hdr.Seq
       c.curSesn = s
       defer func() {
               c.curNmpSeq = 0
               c.curSesn = nil
       }()

       rsp, err := sesn.TxRxMgmt(s, m, c.TxOptions(), rsp_chan)
       if err != nil {
               return nil, err
       }

       return rsp, nil
} */
