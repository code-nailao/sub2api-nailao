import { createApp } from 'vue'
import Intelligence from './Intelligence.vue'
import './intelligence.css'
import '../../../sources-brand/foreground.css'
import { intelligenceAppearance } from './intelligence-appearance'

intelligenceAppearance()
createApp(Intelligence).mount('#app')
