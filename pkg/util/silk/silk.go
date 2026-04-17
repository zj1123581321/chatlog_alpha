package silk

import (
	"fmt"

	"github.com/sjzar/go-lame"
	"github.com/sjzar/go-silk"
)

func Silk2MP3(data []byte) (out []byte, err error) {
	// CGO 层遇到非标准 silk 数据时可能 nil deref；包一层 recover 降级为普通错误，
	// 让上层 handler 走"返回原始字节"的 fallback，而不是整个请求 500。
	defer func() {
		if r := recover(); r != nil {
			out = nil
			err = fmt.Errorf("silk2mp3 panicked: %v", r)
		}
	}()

	sd := silk.SilkInit()
	defer sd.Close()

	pcmdata := sd.Decode(data)
	if len(pcmdata) == 0 {
		return nil, fmt.Errorf("silk decode failed")
	}

	le := lame.Init()
	defer le.Close()

	le.SetInSamplerate(24000)
	le.SetOutSamplerate(24000)
	le.SetNumChannels(1)
	le.SetBitrate(16)
	// IMPORTANT!
	le.InitParams()

	mp3data := le.Encode(pcmdata)
	if len(mp3data) == 0 {
		return nil, fmt.Errorf("mp3 encode failed")
	}

	return mp3data, nil
}
