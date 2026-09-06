package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequireEmployeePrincipalRejectsCompanyUsers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		setPrincipal(c, &Principal{
			Type:      CompanyUserPrincipal,
			CompanyID: "company-id",
			Role:      "super_admin",
		})
		c.Next()
	})
	router.Use(RequireEmployeePrincipal())
	router.GET("/pharmacy", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/pharmacy", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusForbidden, response.Code)
	require.Contains(t, response.Body.String(), "pharmacy_employee_required")
}

func TestRequireEmployeePrincipalAllowsEmployeeWithPharmacy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		setPrincipal(c, &Principal{
			Type:       EmployeePrincipal,
			PharmacyID: "pharmacy-id",
		})
		c.Next()
	})
	router.Use(RequireEmployeePrincipal())
	router.GET("/pharmacy", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/pharmacy", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusNoContent, response.Code)
}
