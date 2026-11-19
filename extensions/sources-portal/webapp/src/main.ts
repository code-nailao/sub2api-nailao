import { createApp } from "vue";
import App from "./App.vue";
import "./style.css";
import { appearance } from "../../../sources-brand/appearance";
import "../../../sources-brand/foreground.css";
appearance();
createApp(App).mount("#app");
