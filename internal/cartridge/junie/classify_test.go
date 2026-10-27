package junie

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"slop-cannon/internal/cartridge"
)

func TestKindForTextPrecedence(t *testing.T) {
	t.Parallel()
	assert.Equal(t, cartridge.KindCreditsExhausted, KindForText("Billing quota exceeded; try again later"))
	assert.Equal(t, cartridge.KindAuth, KindForText("Response 401 | Failed Authentication"))
	assert.Equal(t, cartridge.KindRateLimited, KindForText("Response 429: Too many requests"))
	assert.Equal(t, cartridge.KindUnknown, KindForText("task completed"))
}
