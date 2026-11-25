// Reader tables scroll inside the reading measure and right-align numeric columns.
export function enhanceTables(container) {
  for (const [index, table] of container.querySelectorAll("table").entries()) {
    const scroller = document.createElement("div");
    scroller.className = "table-scroll";
    scroller.tabIndex = 0;
    scroller.setAttribute("role", "region");
    scroller.setAttribute("aria-label", "Table " + (index + 1) + ", scroll horizontally for more columns");
    table.before(scroller);
    scroller.append(table);
    const rows=[...table.tBodies].flatMap(body=>[...body.rows]).map(row=>[...row.cells].map(cell=>cell.textContent));
    for (const column of numericColumns(rows))
      for (const row of table.rows) row.cells[column]?.classList.add("numeric");
  }
}
// Columns whose non-empty body cells are all numbers; the visual editor marks the same ones.
export function numericColumns(rows) {
  const columns=[];
  for (let column = 0; column < Math.max(0, ...rows.map(row=>row.length)); column++) {
    const values=rows.map(row=>row[column]?.trim()||"").filter(Boolean);
    if (values.length && values.every(value=>/^[-+−]?\d[\d.,\s]*%?$/.test(value))) columns.push(column);
  }
  return columns;
}
