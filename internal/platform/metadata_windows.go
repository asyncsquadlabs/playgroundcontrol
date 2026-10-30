//go:build windows

package platform

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/enforce"
)

// Describe adds bounded version-resource metadata to a process. It is called
// only for processes selected for AI classification, not for every scan.
func (s *Source) Describe(process enforce.Process) enforce.Process {
	path := strings.TrimSpace(process.ImagePath)
	if path == "" || strings.HasPrefix(path, `\\`) {
		return process
	}
	product, company, description := versionInfoStrings(path)
	process.ProductName = product
	process.CompanyName = company
	process.FileDescription = description
	return process
}

func versionInfoStrings(path string) (product, company, description string) {
	var zero windows.Handle
	size, err := windows.GetFileVersionInfoSize(path, &zero)
	if err != nil || size == 0 || size > 1<<20 {
		return "", "", ""
	}
	buffer := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buffer[0])); err != nil {
		return "", "", ""
	}
	block := unsafe.Pointer(&buffer[0])
	translations := versionTranslations(block)
	for _, translation := range translations {
		prefix := fmt.Sprintf(`\StringFileInfo\%08x\`, translation)
		product = firstNonEmpty(product, queryVersionString(block, prefix+"ProductName"))
		company = firstNonEmpty(company, queryVersionString(block, prefix+"CompanyName"))
		description = firstNonEmpty(description, queryVersionString(block, prefix+"FileDescription"))
		if product != "" && company != "" && description != "" {
			break
		}
	}
	return cleanVersionString(product), cleanVersionString(company), cleanVersionString(description)
}

func versionTranslations(block unsafe.Pointer) []uint32 {
	var pointer unsafe.Pointer
	var length uint32
	if err := windows.VerQueryValue(block, `\VarFileInfo\Translation`, unsafe.Pointer(&pointer), &length); err != nil ||
		pointer == nil || length < 4 {
		return nil
	}
	count := int(length) / 4
	if count > 64 {
		count = 64
	}
	out := make([]uint32, 0, count)
	for i := 0; i < count; i++ {
		lang := *(*uint16)(unsafe.Add(pointer, i*4))
		codepage := *(*uint16)(unsafe.Add(pointer, i*4+2))
		out = append(out, uint32(lang)<<16|uint32(codepage))
	}
	if len(out) == 0 {
		return []uint32{0x040904B0}
	}
	return out
}

func queryVersionString(block unsafe.Pointer, key string) string {
	var pointer unsafe.Pointer
	var length uint32
	if err := windows.VerQueryValue(block, key, unsafe.Pointer(&pointer), &length); err != nil ||
		pointer == nil || length == 0 {
		return ""
	}
	if length > 1024 {
		length = 1024
	}
	value := windows.UTF16ToString(unsafe.Slice((*uint16)(pointer), int(length)))
	return strings.TrimSpace(value)
}

func cleanVersionString(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 200 {
		value = value[:200]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
}

func firstNonEmpty(current, candidate string) string {
	if strings.TrimSpace(current) != "" {
		return current
	}
	return candidate
}
