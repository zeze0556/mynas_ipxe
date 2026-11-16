package tftp

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Serve 启动最小化 TFTP 服务，从 bootDir 提供 iPXE 引导文件（仅 RRQ/只读）。
// 主循环只在 listen 端口收 RRQ，每个传输由独立的 per-transfer UDP socket
// （标准 TFTP 的随机 TID 设计）完成 DATA/ACK，避免单 socket 多线程抢读导致
// ACK 被主循环吞掉而卡死。
func Serve(listen, bootDir string) error {
	pc, err := net.ListenPacket("udp", listen)
	if err != nil {
		return fmt.Errorf("tftp listen %s: %w", listen, err)
	}
	defer pc.Close()
	log.Printf("[tftp] listening %s, root=%s", listen, bootDir)
	buf := make([]byte, 516)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			log.Printf("[tftp] read error: %v", err)
			continue
		}
		if n < 2 {
			continue
		}
		opcode := binary.BigEndian.Uint16(buf[0:2])
		if opcode != 1 { // 仅支持 RRQ
			// 用临时连接回错误，避免占用主监听 socket
			if c, e := net.Dial("udp", addr.String()); e == nil {
				sendErrorC(c, 1, "write not supported")
				c.Close()
			}
			continue
		}
		filename, opts, ok := parseRRQ(buf[2:n])
		if !ok {
			if c, e := net.Dial("udp", addr.String()); e == nil {
				sendErrorC(c, 1, "bad request")
				c.Close()
			}
			continue
		}
		go handleRead(addr, bootDir, filename, opts)
	}
}

func parseRRQ(b []byte) (string, map[string]string, bool) {
	parts := strings.Split(string(b), "\x00")
	if len(parts) < 2 || parts[0] == "" {
		return "", nil, false
	}
	opts := map[string]string{}
	for i := 2; i+1 < len(parts); i += 2 {
		opts[strings.ToLower(parts[i])] = parts[i+1]
	}
	return parts[0], opts, true
}

func buildOACK(blksize int, opts map[string]string, size int64) []byte {
	b := []byte{0, 6}
	if _, ok := opts["blksize"]; ok {
		b = append(b, []byte("blksize\x00"+strconv.Itoa(blksize)+"\x00")...)
	}
	if _, ok := opts["tsize"]; ok {
		b = append(b, []byte("tsize\x00"+strconv.FormatInt(size, 10)+"\x00")...)
	}
	return b
}

func handleRead(client net.Addr, bootDir, filename string, opts map[string]string) {
	conn, err := net.Dial("udp", client.String())
	if err != nil {
		return
	}
	defer conn.Close()

	clean := filepath.Clean("/" + filename)
	p := filepath.Join(bootDir, clean)
	log.Printf("[tftp] req=%q -> p=%q", filename, p)
	if !within(bootDir, p) {
		log.Printf("[tftp] forbidden path: %s", filename)
		sendErrorC(conn, 2, "access violation")
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		log.Printf("[tftp] open failed: %q err=%v", p, err)
		sendErrorC(conn, 1, "file not found")
		return
	}

	blksize := 512
	if v, ok := opts["blksize"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 8 && n <= 65464 {
			blksize = n
		}
	}

	// 客户端带选项时先发 OACK 并等待 ACK(0)。
	if len(opts) > 0 {
		oack := buildOACK(blksize, opts, int64(len(data)))
		acked := false
		for attempt := 0; attempt < 5; attempt++ {
			conn.Write(oack)
			if _, err := readACKC(conn, 3*time.Second); err == nil {
				acked = true
				break
			}
		}
		if !acked {
			log.Printf("[tftp] oack not acked: %s", filename)
			return
		}
	}

	block := uint16(1)
	pos := 0
	for {
		end := pos + blksize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[pos:end]
		pkt := make([]byte, 4+len(chunk))
		binary.BigEndian.PutUint16(pkt[0:2], 3) // DATA
		binary.BigEndian.PutUint16(pkt[2:4], block)
		copy(pkt[4:], chunk)

		acked := false
		for attempt := 0; attempt < 5; attempt++ {
			conn.Write(pkt)
			ack, err := readACKC(conn, 3*time.Second)
			if err != nil {
				continue // 超时重传
			}
			if ack == block {
				acked = true
				break
			}
			if ack == block-1 { // 客户端未收到本块，重传
				continue
			}
		}
		if !acked {
			log.Printf("[tftp] give up block %d: %s", block, filename)
			return
		}
		pos = end
		if len(chunk) < blksize {
			break // 末块
		}
		block++
	}
	log.Printf("[tftp] sent %s (%d bytes)", filename, len(data))
}

func readACKC(conn net.Conn, timeout time.Duration) (uint16, error) {
	conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})
	buf := make([]byte, 516)
	n, err := conn.Read(buf)
	if err != nil {
		return 0, err
	}
	if n < 4 {
		return 0, fmt.Errorf("short packet")
	}
	if binary.BigEndian.Uint16(buf[0:2]) != 4 { // 非 ACK
		return 0, fmt.Errorf("not ack")
	}
	return binary.BigEndian.Uint16(buf[2:4]), nil
}

func sendErrorC(conn net.Conn, code uint16, msg string) {
	pkt := make([]byte, 4+len(msg)+1)
	binary.BigEndian.PutUint16(pkt[0:2], 5) // ERROR
	binary.BigEndian.PutUint16(pkt[2:4], code)
	copy(pkt[4:], msg)
	pkt[len(pkt)-1] = 0
	conn.Write(pkt)
}

// within 判断 target 是否落在 base 内（防路径穿越）。
func within(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
