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
	"crypto/sha256"
	"fmt"

	"github.com/cheggaaa/pb"

	"mynewt.apache.org/newtmgr/nmxact/mgmt"
	"mynewt.apache.org/newtmgr/nmxact/nmp"
	"mynewt.apache.org/newtmgr/nmxact/nmxutil"
	"mynewt.apache.org/newtmgr/nmxact/sesn"
	log "github.com/sirupsen/logrus"
    "container/list"
    "sync"
//    "time"
//    rt "runtime/debug"
)

//////////////////////////////////////////////////////////////////////////////
// $upload                                                                  //
//////////////////////////////////////////////////////////////////////////////
const IMAGE_UPLOAD_MAX_CHUNK = 512
const IMAGE_UPLOAD_MIN_1ST_CHUNK = 32

type ImageUploadProgressFn func(c *ImageUploadCmd, r *nmp.ImageUploadRsp)
type ImageUploadCmd struct {
	CmdBase
	Data       []byte
	StartOff   int
	Upgrade    bool
	ProgressCb ImageUploadProgressFn
	ImageNum   int
}

type ImageUploadResult struct {
	Rsps []*nmp.ImageUploadRsp
}

func NewImageUploadCmd() *ImageUploadCmd {
	return &ImageUploadCmd{
		CmdBase: NewCmdBase(),
	}
}

func newImageUploadResult() *ImageUploadResult {
	return &ImageUploadResult{}
}

func (r *ImageUploadResult) Status() int {
	if len(r.Rsps) > 0 {
		return r.Rsps[len(r.Rsps)-1].Rc
	} else {
		return nmp.NMP_ERR_EUNKNOWN
	}
}

func buildImageUploadReq(imageSz int, hash []byte, upgrade bool, chunk []byte,
	off int, imageNum int, seq uint8) *nmp.ImageUploadReq {

	r := nmp.NewImageUploadReqWithSeq(seq)

	if off == 0 {
		r.Len = uint32(imageSz)
		r.DataSha = hash
		r.Upgrade = upgrade
	}
	r.Off = uint32(off)
	r.Data = chunk
	r.ImageNum = uint8(imageNum)

	return r
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func encodeUploadReq(s sesn.Sesn, hash []byte, upgrade bool, data []byte,
	off int, chunklen int, imageNum int, seq uint8) ([]byte, error) {

	r := buildImageUploadReq(len(data), hash, upgrade, data[off:off+chunklen],
		off, imageNum, seq)
	enc, err := mgmt.EncodeMgmt(s, r.Msg())
	if err != nil {
		return nil, err
	}

	return enc, nil
}

func findChunkLen(s sesn.Sesn, hash []byte, upgrade bool, data []byte,
	off int, imageNum int, seq uint8) (int, error) {

	// Let's start by encoding max allowed chunk len and we will see how many
	// bytes we need to cut
	chunklen := min(len(data)-off, IMAGE_UPLOAD_MAX_CHUNK)

	// Keep reducing the chunk size until the request fits the MTU.
	for {
		enc, err := encodeUploadReq(s, hash, upgrade, data, off, chunklen, imageNum, seq)
		if err != nil {
			return 0, err
		}

		if len(enc) <= s.MtuOut() {
			break
		}

		// Encoded length is larger than MTU, we need to make chunk shorter
		overflow := len(enc) - s.MtuOut()
		chunklen -= overflow
	}

	return chunklen, nil
}

func nextImageUploadReq(s sesn.Sesn, upgrade bool, data []byte, off int, imageNum int) (
	*nmp.ImageUploadReq, error) {
	var hash []byte = nil

	// For 1st chunk we'll need valid data hash
	if off == 0 {
		sha := sha256.Sum256(data)
		hash = sha[:]
	}

	seq := nmxutil.NextNmpSeq()

	// Find chunk length
	chunklen, err := findChunkLen(s, hash, upgrade, data, off, imageNum, seq)
	if err != nil {
		return nil, err
	}

	// For 1st chunk we need to send at least full header so if it does not
	// fit we'll recalculate without hash
	if off == 0 && chunklen < IMAGE_UPLOAD_MIN_1ST_CHUNK {
		hash = nil
		chunklen, err = findChunkLen(s, hash, upgrade, data, off, imageNum, seq)
		if err != nil {
			return nil, err
		}
	}

	// If calculated chunk length is not enough to send at least single byte
	// we can't do much more...
	if chunklen <= 0 {
		return nil, fmt.Errorf("Cannot create image upload request; "+
			"MTU too low to fit any image data; max-payload-size=%d",
			s.MtuOut())
	}

    log.Debugf("Chunklen %d, range %d to %d", chunklen, off, off+chunklen)
	r := buildImageUploadReq(len(data), hash, upgrade,
		data[off:off+chunklen], off, imageNum, seq)

	// Request above should encode just fine since we calculate proper chunk
	// length but (at least for now) let's double check it
	enc, err := mgmt.EncodeMgmt(s, r.Msg())
	if err != nil {
		return nil, err
	}
	if len(enc) > s.MtuOut() {
		return nil, fmt.Errorf("Invalid chunk length; payload-size=%d "+
			"max-payload-size=%d", len(enc), s.MtuOut())
	}

	return r, nil
}


func (c *ImageUploadCmd) Run(s sesn.Sesn) (Result, error) {
    res := newImageUploadResult()
    sem := make(chan int, 2)
    queue := list.New()
    var mutex = &sync.Mutex{}


	for off := c.StartOff; off < len(c.Data); {
        sem <- 1
        mutex.Lock()
		r, err := nextImageUploadReq(s, c.Upgrade, c.Data, off, c.ImageNum)
		if err != nil {
            mutex.Unlock()
			return nil, err
		}
        queue.PushBack(r)
        log.Debugf("PushBack r.off %d r.len %d sem len %d", r.Off, r.Len, len(sem))
        mutex.Unlock()

        //increment offset to previous + size of last
        off = (int(r.Off) + len(r.Data))

        go func() {
            var _r *nmp.ImageUploadReq
            err_c := make(chan error, 1)
            rsp_c := make(chan nmp.NmpRsp, 1)
            defer close(err_c)
            defer close(rsp_c)

            mutex.Lock()
            if (queue.Len() > 0) {
                ele := queue.Front()
                queue.Remove(ele)
	            _r = ele.Value.(*nmp.ImageUploadReq)
                log.Debugf("_r.off %d _r.len %d", _r.Off, _r.Len)
            }
            txReq_async(s, _r.Msg(), &c.CmdBase, rsp_c, err_c)

            for {
                select {
                    case err := <- err_c:
                        log.Debugf("txReq err %v", err)
                        <-sem
                        mutex.Unlock()
                        return
                    case rsp := <- rsp_c:
                        log.Debugf("txReq complete %v", rsp)
                        irsp := rsp.(*nmp.ImageUploadRsp)
                        res.Rsps = append(res.Rsps, irsp)
			            log.Debugf("resp returned (next) offset %d", int(irsp.Off))

                        if c.ProgressCb != nil {
                            c.ProgressCb(c, irsp)
                        }
                        <-sem
                        mutex.Unlock()
               }
           }
       }()


	}

	return res, nil
}

//////////////////////////////////////////////////////////////////////////////
// $upgrade                                                                 //
//////////////////////////////////////////////////////////////////////////////

// Image upgrade combines the image erase and image upload commands into a
// single command.  Some hardware and / or BLE connection settings cause the
// connection to drop while flash is being erased or written to.  The image
// upgrade command addresses this issue with the following sequence:
// 1. Send image erase command.
// 2. If the image erase command succeeded, proceed to step 5.
// 3. Else if the peer is disconnected, attempt to reconnect to the peer.  If
//    the reconnect attempt fails, abort the command and report the error.  If
//    the reconnect attempt succeeded, proceed to step 5.
// 4. Else (the erase command failed and the peer is still connected), proceed
//    to step 5.
// 5. Execute the upload command.  If the connection drops before the final
//    part is uploaded, reconnect and retry the previous part.

type ImageUpgradeCmd struct {
	CmdBase
	Data        []byte
	NoErase     bool
	ProgressCb  ImageUploadProgressFn
	LastOff     uint32
	Upgrade     bool
	ProgressBar *pb.ProgressBar
	ImageNum    int
}

type ImageUpgradeResult struct {
	EraseRes  *ImageEraseResult
	UploadRes *ImageUploadResult
}

func NewImageUpgradeCmd() *ImageUpgradeCmd {
	return &ImageUpgradeCmd{
		CmdBase:  NewCmdBase(),
		NoErase:  false,
		ImageNum: 0,
	}
}

func newImageUpgradeResult() *ImageUpgradeResult {
	return &ImageUpgradeResult{}
}

func (r *ImageUpgradeResult) Status() int {
	if r.UploadRes != nil {
		return r.UploadRes.Status()
	} else if r.EraseRes != nil {
		return r.EraseRes.Status()
	} else {
		return nmp.NMP_ERR_EUNKNOWN
	}
}

// Attempts to recover from a disconnect.
func (c *ImageUpgradeCmd) rescue(s sesn.Sesn, err error) error {
	if err != nil {
		if !s.IsOpen() {
			if err := s.Open(); err == nil {
				return nil
			}
		}
	}

	return err
}

func (c *ImageUpgradeCmd) runErase(s sesn.Sesn) (*ImageEraseResult, error) {
	cmd := NewImageEraseCmd()
	cmd.SetTxOptions(c.TxOptions())
	res, err := cmd.Run(s)

	if err := c.rescue(s, err); err != nil {
		return nil, err
	}

	if res == nil {
		// We didn't get a response back but we rescued ourselves from the
		// disconnect.
		res = newImageEraseResult()
	}

	return res.(*ImageEraseResult), nil
}

func (c *ImageUpgradeCmd) runUpload(s sesn.Sesn) (*ImageUploadResult, error) {
	startOff := 0
	progressCb := func(uc *ImageUploadCmd, r *nmp.ImageUploadRsp) {
		if r.Rc == 0 {
			startOff = int(r.Off)
		}
		c.ProgressCb(uc, r)
	}

	for {
		cmd := NewImageUploadCmd()
		cmd.Data = c.Data
		cmd.StartOff = startOff
		cmd.Upgrade = c.Upgrade
		cmd.ProgressCb = progressCb
		cmd.ImageNum = c.ImageNum
		cmd.SetTxOptions(c.TxOptions())

		res, err := cmd.Run(s)
		if err == nil {
			return res.(*ImageUploadResult), nil
		}

		if err := c.rescue(s, err); err != nil {
			// Disconnected and couldn't recover.
			return nil, err
		}

		// Disconnected but recovered; retry last part.
	}
}

func (c *ImageUpgradeCmd) Run(s sesn.Sesn) (Result, error) {
	var eres *ImageEraseResult = nil
	var err error

	if c.NoErase == false {
		eres, err = c.runErase(s)
		if err != nil {
			return nil, err
		}
	} else {
		eres = nil
	}
	ures, err := c.runUpload(s)
	if err != nil {
		return nil, err
	}

	upgradeRes := newImageUpgradeResult()
	upgradeRes.EraseRes = eres
	upgradeRes.UploadRes = ures
	return upgradeRes, nil
}

//////////////////////////////////////////////////////////////////////////////
// $state read                                                              //
//////////////////////////////////////////////////////////////////////////////

type ImageStateReadCmd struct {
	CmdBase
}

type ImageStateReadResult struct {
	Rsp *nmp.ImageStateRsp
}

func NewImageStateReadCmd() *ImageStateReadCmd {
	return &ImageStateReadCmd{
		CmdBase: NewCmdBase(),
	}
}

func newImageStateReadResult() *ImageStateReadResult {
	return &ImageStateReadResult{}
}

func (r *ImageStateReadResult) Status() int {
	return r.Rsp.Rc
}

func (c *ImageStateReadCmd) Run(s sesn.Sesn) (Result, error) {
	r := nmp.NewImageStateReadReq()

	rsp, err := txReq(s, r.Msg(), &c.CmdBase)
	if err != nil {
		return nil, err
	}
	srsp := rsp.(*nmp.ImageStateRsp)

	res := newImageStateReadResult()
	res.Rsp = srsp
	return res, nil
}

//////////////////////////////////////////////////////////////////////////////
// $state write                                                             //
//////////////////////////////////////////////////////////////////////////////

type ImageStateWriteCmd struct {
	CmdBase
	Hash    []byte
	Confirm bool
}

type ImageStateWriteResult struct {
	Rsp *nmp.ImageStateRsp
}

func NewImageStateWriteCmd() *ImageStateWriteCmd {
	return &ImageStateWriteCmd{
		CmdBase: NewCmdBase(),
	}
}

func newImageStateWriteResult() *ImageStateWriteResult {
	return &ImageStateWriteResult{}
}

func (r *ImageStateWriteResult) Status() int {
	return r.Rsp.Rc
}

func (c *ImageStateWriteCmd) Run(s sesn.Sesn) (Result, error) {
	r := nmp.NewImageStateWriteReq()
	r.Hash = c.Hash
	r.Confirm = c.Confirm

	rsp, err := txReq(s, r.Msg(), &c.CmdBase)
	if err != nil {
		return nil, err
	}
	srsp := rsp.(*nmp.ImageStateRsp)

	res := newImageStateWriteResult()
	res.Rsp = srsp
	return res, nil
}

//////////////////////////////////////////////////////////////////////////////
// $corelist                                                                //
//////////////////////////////////////////////////////////////////////////////

type CoreListCmd struct {
	CmdBase
}

type CoreListResult struct {
	Rsp *nmp.CoreListRsp
}

func NewCoreListCmd() *CoreListCmd {
	return &CoreListCmd{
		CmdBase: NewCmdBase(),
	}
}

func newCoreListResult() *CoreListResult {
	return &CoreListResult{}
}

func (r *CoreListResult) Status() int {
	return r.Rsp.Rc
}

func (c *CoreListCmd) Run(s sesn.Sesn) (Result, error) {
	r := nmp.NewCoreListReq()

	rsp, err := txReq(s, r.Msg(), &c.CmdBase)
	if err != nil {
		return nil, err
	}
	srsp := rsp.(*nmp.CoreListRsp)

	res := newCoreListResult()
	res.Rsp = srsp
	return res, nil
}

//////////////////////////////////////////////////////////////////////////////
// $erase                                                                   //
//////////////////////////////////////////////////////////////////////////////

type ImageEraseCmd struct {
	CmdBase
}

type ImageEraseResult struct {
	Rsp *nmp.ImageEraseRsp
}

func NewImageEraseCmd() *ImageEraseCmd {
	return &ImageEraseCmd{
		CmdBase: NewCmdBase(),
	}
}

func newImageEraseResult() *ImageEraseResult {
	return &ImageEraseResult{}
}

func (r *ImageEraseResult) Status() int {
	return r.Rsp.Rc
}

func (c *ImageEraseCmd) Run(s sesn.Sesn) (Result, error) {
	r := nmp.NewImageEraseReq()

	rsp, err := txReq(s, r.Msg(), &c.CmdBase)
	if err != nil {
		return nil, err
	}
	srsp := rsp.(*nmp.ImageEraseRsp)

	res := newImageEraseResult()
	res.Rsp = srsp
	return res, nil
}

//////////////////////////////////////////////////////////////////////////////
// $coreload                                                                //
//////////////////////////////////////////////////////////////////////////////

type CoreLoadProgressFn func(c *CoreLoadCmd, r *nmp.CoreLoadRsp)
type CoreLoadCmd struct {
	CmdBase
	ProgressCb CoreLoadProgressFn
}

type CoreLoadResult struct {
	Rsps []*nmp.CoreLoadRsp
}

func NewCoreLoadCmd() *CoreLoadCmd {
	return &CoreLoadCmd{
		CmdBase: NewCmdBase(),
	}
}

func newCoreLoadResult() *CoreLoadResult {
	return &CoreLoadResult{}
}

func (r *CoreLoadResult) Status() int {
	rsp := r.Rsps[len(r.Rsps)-1]
	return rsp.Rc
}

func (c *CoreLoadCmd) Run(s sesn.Sesn) (Result, error) {
	res := newCoreLoadResult()
	off := 0

	for {
		r := nmp.NewCoreLoadReq()
		r.Off = uint32(off)

		rsp, err := txReq(s, r.Msg(), &c.CmdBase)
		if err != nil {
			return nil, err
		}
		irsp := rsp.(*nmp.CoreLoadRsp)

		if c.ProgressCb != nil {
			c.ProgressCb(c, irsp)
		}

		res.Rsps = append(res.Rsps, irsp)
		if irsp.Rc != 0 {
			break
		}

		if len(irsp.Data) == 0 {
			// Download complete.
			break
		}

		off = int(irsp.Off) + len(irsp.Data)
	}

	return res, nil
}

//////////////////////////////////////////////////////////////////////////////
// $coreerase                                                               //
//////////////////////////////////////////////////////////////////////////////

type CoreEraseCmd struct {
	CmdBase
}

type CoreEraseResult struct {
	Rsp *nmp.CoreEraseRsp
}

func NewCoreEraseCmd() *CoreEraseCmd {
	return &CoreEraseCmd{
		CmdBase: NewCmdBase(),
	}
}

func newCoreEraseResult() *CoreEraseResult {
	return &CoreEraseResult{}
}

func (r *CoreEraseResult) Status() int {
	return r.Rsp.Rc
}

func (c *CoreEraseCmd) Run(s sesn.Sesn) (Result, error) {
	r := nmp.NewCoreEraseReq()

	rsp, err := txReq(s, r.Msg(), &c.CmdBase)
	if err != nil {
		return nil, err
	}
	srsp := rsp.(*nmp.CoreEraseRsp)

	res := newCoreEraseResult()
	res.Rsp = srsp
	return res, nil
}
