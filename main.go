package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net"
	"os/exec"
	"syscall"
	"unsafe"
)

type ifreq struct {
	Name  [16]byte
	Flags uint16
	pad   [22]byte
}

func main() {
	// --- 1. PENGATURAN PARAMETER CLI ---
	localUDP := flag.String("local", ":8000", "Port UDP lokal untuk mendengarkan")
	remoteUDP := flag.String("remote", "192.168.0.152:8000", "IP & Port UDP tujuan")
	tunIP := flag.String("tun", "10.0.0.1/24", "IP Address bohongan untuk jaringan VPN (TUN)")
	secretKey := flag.String("key", "BabelGatewaySecretKey32BytesLong", "Kunci Enkripsi 32-byte (AES-256)")
	flag.Parse()

	if len(*secretKey) != 32 {
		log.Fatal("Secret key harus tepat 32 karakter agar AES-256 bisa bekerja!")
	}

	// --- 2. PEMBUATAN INTERFACE TUN (RAW SYSTEM CALL) ---
	// Kita gunakan syscall.Open alih-alih os.OpenFile untuk menghindari error 'not pollable' dari Golang
	tunFd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR, 0)
	if err != nil {
		log.Fatalf("Gagal membuka /dev/net/tun: %v (Apakah kamu lupa pakai sudo?)", err)
	}
	defer syscall.Close(tunFd)

	var req ifreq
	copy(req.Name[:], "tun0") // Nama interface virtual kita: tun0
	req.Flags = 0x0001 | 0x1000 

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(tunFd), syscall.TUNSETIFF, uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		log.Fatalf("Sistem operasi menolak pembuatan antarmuka TUN: %v", errno)
	}

	exec.Command("ip", "link", "set", "dev", "tun0", "up").Run()
	exec.Command("ip", "addr", "add", *tunIP, "dev", "tun0").Run()
	fmt.Printf("[+] Antarmuka VPN (tun0) berhasil diaktifkan dengan IP: %s\n", *tunIP)

	// --- 3. SETUP JALUR INTERNET PUBLIC (UDP) & KRIPTOGRAFI ---
	localAddr, _ := net.ResolveUDPAddr("udp", *localUDP)
	remoteAddr, _ := net.ResolveUDPAddr("udp", *remoteUDP)

	conn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		log.Fatalf("Gagal membuka socket UDP: %v", err)
	}
	defer conn.Close()
	fmt.Printf("[+] Mendengarkan Public Traffic di UDP %s\n", *localUDP)
	fmt.Printf("[+] Target Remote (Lawan) berada di UDP %s\n\n", *remoteUDP)

	block, _ := aes.NewCipher([]byte(*secretKey))
	gcm, _ := cipher.NewGCM(block)

	// --- 4. ENGINE VPN (MULTITHREADING / GOROUTINES) ---

	// Pekerja A: Menerima UDP -> Dekripsi -> Suntik ke Kernel
	go func() {
		udpBuf := make([]byte, 4096)
		for {
			n, _, err := conn.ReadFromUDP(udpBuf)
			if err != nil || n < 12 {
				continue 
			}

			nonce := udpBuf[:12]        
			ciphertext := udpBuf[12:n]  

			plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
			if err != nil {
				log.Println("[!] Peringatan: Menerima paket cacat/disusupi (Gagal Didekripsi)!")
				continue
			}

			// Injeksi Native Traffic menggunakan Raw Syscall Write
			syscall.Write(tunFd, plaintext)
		}
	}()

	// Pekerja B: Menangkap TUN -> Enkripsi -> Lempar ke UDP
	tunBuf := make([]byte, 4096)
	for {
		// Menangkap Native Traffic menggunakan Raw Syscall Read
		n, err := syscall.Read(tunFd, tunBuf)
		if err != nil {
			log.Fatalf("Gagal membaca traffic Linux: %v", err)
		}

		packetRaw := tunBuf[:n]

		nonce := make([]byte, 12)
		rand.Read(nonce)

		ciphertext := gcm.Seal(nil, nonce, packetRaw, nil)

		payload := append(nonce, ciphertext...)
		conn.WriteToUDP(payload, remoteAddr)
	}
}