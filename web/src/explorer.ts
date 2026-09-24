import SwaggerUI from "swagger-ui-dist/swagger-ui-bundle.js";
import "swagger-ui-dist/swagger-ui.css";
import "./explorer.css";

const manufacturer =
  new URLSearchParams(location.search).get("api") === "manufacturer";
SwaggerUI({
  dom_id: "#swagger-ui",
  url: manufacturer ? "/manufacturer-openapi.yaml" : "/openapi.yaml",
  supportedSubmitMethods: manufacturer
    ? []
    : ["get", "post", "put", "delete", "patch", "head", "options"],
  validatorUrl: "none",
  queryConfigEnabled: false,
  withCredentials: true,
  persistAuthorization: false,
  deepLinking: true,
  filter: true,
  displayRequestDuration: true,
  docExpansion: "list",
});
