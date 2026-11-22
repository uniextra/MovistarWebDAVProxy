package sapi

import "testing"

// Las URL firmadas de descarga se envían byte a byte como llegaron del listado:
// cambiar su validationkey o reordenar la query invalida la firma (403).
func TestAppendValidationKey_KeepsSignedURL(t *testing.T) {
	c := &Client{}
	c.cfg.ValidationKey = "actual"
	signed := "https://micloud.movistar.es/sapi/download/file?z=1&validationkey=antigua&a=b%2Fc&signature=x"
	if got := c.appendValidationKey(signed); got != signed {
		t.Fatalf("URL firmada modificada:\n got %s\nwant %s", got, signed)
	}
	if got := c.appendValidationKey("https://h/sapi/media?action=get&b=2"); got != "https://h/sapi/media?action=get&b=2&validationkey=actual" {
		t.Fatalf("añadir validationkey: %s", got)
	}
	if got := c.replaceValidationKey("https://h/sapi/media?action=get&validationkey=antigua"); got != "https://h/sapi/media?action=get&validationkey=actual" {
		t.Fatalf("reintento: %s", got)
	}
}
