import { use } from 'echarts/core'
import { LineChart, BarChart, PieChart, ScatterChart } from 'echarts/charts'
import { GridComponent, TooltipComponent, LegendComponent, TitleComponent, DataZoomComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'

use([LineChart, BarChart, PieChart, ScatterChart, GridComponent, TooltipComponent, LegendComponent, TitleComponent, DataZoomComponent, CanvasRenderer])
export { init, use, graphic, getInstanceByDom } from 'echarts/core'
