// The standalone distribution uses the same configuration API as swagger-ui.
declare module "swagger-ui-dist/swagger-ui-bundle.js" {
  import SwaggerUI from "swagger-ui";
  export default SwaggerUI;
}
