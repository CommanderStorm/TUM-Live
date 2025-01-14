import { Get } from "./global";
import { Calendar } from "@fullcalendar/core";
import dayGridPlugin from "@fullcalendar/daygrid";
import timeGridPlugin from "@fullcalendar/timegrid";
import iCalendarPlugin from "@fullcalendar/icalendar";
import { ex } from "@fullcalendar/core/internal-common";

let calendar: Calendar;

export function addScheduleListener(lectureHalls: number[]) {
    document.addEventListener("DOMContentLoaded", function () {
        const calendarEl = document.getElementById("calendar");
        // Init fullcallendar
        opts.events.url = "/api/schedule.ics?lecturehalls=" + lectureHalls.join(",");
        calendar = new Calendar(calendarEl, opts);
        calendar.render();
    });
}

export function refetchCalendar(lecturehalls: number[]) {
    opts.events.url = "/api/schedule.ics?lecturehalls=" + lecturehalls.join(",");
    calendar.removeAllEventSources();
    calendar.addEventSource(opts.events);
}

const opts = {
    plugins: [dayGridPlugin, timeGridPlugin, iCalendarPlugin],
    headerToolbar: { center: "timeGridDay,timeGridWeek", right: "lhSelect prev,next" },
    initialView: "timeGridDay",
    nowIndicator: true,
    firstDay: 1,
    height: "85vh",
    allDaySlot: false,
    events: {
        url: "/api/schedule.ics",
        format: "ics",
    },
    customButtons: {
        lhSelect: {
            text: "LectureHalls",
            click: function () {
                window.dispatchEvent(new CustomEvent("showlhselect"));
            },
        },
    },
    eventDidMount: function (e) {
        // manipulate dom element on event rendering -> inject events location
        e.el.title = e.event.title;
        const eventLocation = e.event.extendedProps.location;
        if (eventLocation !== null && eventLocation !== undefined && eventLocation !== "") {
            e.el.title = e.el.title + " Location: " + eventLocation;
            const locationElem = document.createElement("i");
            locationElem.innerHTML = "&#183;" + eventLocation;
            e.el.getElementsByClassName("fc-event-time")[0].appendChild(locationElem);
        }
    },
    eventClick: function (data) {
        // load some extra info on click
        const popover = document.getElementById("popoverContent");
        const streamInfo = JSON.parse(Get("/api/stream/" + data.event.extendedProps.description));
        popover.innerHTML = `
            <p class="flex text-1 text-lg">
                <span class="grow">${streamInfo["course"]}</span>
                <i id="closeBtn" class="transition-colors duration-200 hover:text-1 text-4 icon-close"></i>
            </p>
                <div class="text-2">
                    <div class="flex"><p>${new Date(streamInfo["start"]).toLocaleString()}</p></div>
                </div>
                <form x-data="{showSubmit:false}" @submit="admin.saveLectureName(event, ${streamInfo["courseID"]}, ${
            streamInfo["streamID"]
        }).then((r)=>showSubmit=!r)"
                    class="w-full flex flex-row mb-2 focus-within:border-gray-300 border-gray-500">
                    <label for="lectureNameInput${streamInfo["streamID"]}" class="hidden">Lecture title</label>
                    <input id="lectureNameInput${streamInfo["streamID"]}"
                        @change="showSubmit=true" @keyup="showSubmit=true"
                        class="tl-input grow border-none" type="text" value="${streamInfo["name"]}"
                        placeholder="Lecture 2: Dark-Patterns I"
                        autocomplete="off">
                    <button x-show="showSubmit" id="nameSubmitBtn${streamInfo["streamID"]}"
                        class="fas fa-check ml-2 text-gray-400 hover:text-purple-500"></button>
                </form>
                <form x-data="{showSubmit:false}" @submit="admin.saveLectureDescription(event, ${
                    streamInfo["courseID"]
                }, ${streamInfo["streamID"]}).then((r)=>showSubmit=!r)"
                    class="w-full flex flex-row focus-within:border-gray-300 border-gray-500">
                    <label for="lectureDescriptionInput${
                        streamInfo["streamID"]
                    }" class="hidden">Lecture description</label>
                    <textarea id="lectureDescriptionInput${streamInfo["streamID"]}"
                        rows="3"
                        @change="showSubmit=true" @keyup="showSubmit=true"
                        class="tl-input grow border-none"
                        placeholder="Add a nice description, links, and more. You can use Markdown."
                        autocomplete="off">${streamInfo["description"]}</textarea>
                    <button x-show="showSubmit" id="descriptionSubmitBtn${streamInfo["streamID"]}"
                        class="fas fa-check ml-2 text-4 hover:text-1"></button>
                </form>
            <a class="text-3 hover:text-black dark:hover:text-white" href="/admin/course/${
                streamInfo["courseID"]
            }#lecture-li-${streamInfo["streamID"]}">Edit <i class="fas fa-external-link-alt"></i></a>
            `;
        document.getElementsByClassName("fc-timegrid").item(0)?.classList.add("filter", "blur-xxs");
        popover.classList.remove("hidden");
        document.getElementById("closeBtn").onclick = () => {
            document.getElementsByClassName("fc-timegrid").item(0)?.classList.remove("filter", "blur-xxs");
            popover.classList.add("hidden");
            this.render();
        };
        this.render();
    },
};

export function toggleAllLectureHalls() {
    const toggles = document.getElementsByClassName("lh-toggle");
    let allSelected = true;
    for (let i = 0; i < toggles.length; i++) {
        if (!(toggles.item(i) as HTMLInputElement).checked) {
            allSelected = false;
            break;
        }
    }
    for (let i = 0; i < toggles.length; i++) {
        if (
            (allSelected && (toggles.item(i) as HTMLInputElement).checked) ||
            (!allSelected && !(toggles.item(i) as HTMLInputElement).checked)
        ) {
            (toggles.item(i) as HTMLInputElement).click();
        }
    }
}
